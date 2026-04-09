package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
	"github.com/user/ff14rader/internal/scoring"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/plugin/dbresolver"
)

// SyncManager 同步管理器
type SyncManager struct {
	client *FFLogsClient
	scorer *scoring.Service
}

func NewSyncManager(client *FFLogsClient) *SyncManager {
	return &SyncManager{client: client, scorer: scoring.NewServiceFromEnv()}
}

// StartIncrementalSync 开始增量同步
func (s *SyncManager) StartIncrementalSync(ctx context.Context, name, server, region string) error {
	log.Printf("开始同步玩家 [%s-%s] 的数据...", name, server)
	region = "CN"
	newlyCreated := false

	resolvedID, created, err := ensurePlayerID(name, server, region)
	if err != nil {
		return fmt.Errorf("resolve player failed: %v", err)
	}
	playerID := resolvedID
	newlyCreated = created

	allowV2Query, reason, err := shouldRunV2ReportQuery(playerID, newlyCreated)
	if err != nil {
		return fmt.Errorf("check v2 query gate failed: %v", err)
	}
	if !allowV2Query {
		log.Printf("[INFO] 跳过 V2 reports 查询: %s", reason)
		if _, err := s.downloadV1Reports(ctx, playerID); err != nil {
			return err
		}
		return s.refreshPlayerOutputAbilityFromLogs(ctx, playerID, name, server, region)
	}
	log.Printf("[INFO] 执行 V2 reports 查询: %s", reason)

	// 确保去重键的唯一索引存在（避免并发创建重复 master 行）
	// (name, timestamp) 足以区分同一场战斗
	_ = db.DB.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_fight_sync_maps_key ON fight_sync_maps (name, timestamp)")
	_ = db.DB.Exec("CREATE INDEX IF NOT EXISTS idx_fight_sync_maps_source_ids_gin ON fight_sync_maps USING GIN (source_ids)")

	// 启动时先自检：若 fights 已全下载但 reports 仍未标记，先修复一次
	if err := s.selfHealReportDownloadStatus(playerID); err != nil {
		return err
	}

	// 1. 获取数据库中该玩家最后一条记录的时间
	var lastSync models.FightSyncMap
	var startTime int64 = 0

	// 允许通过环境变量或配置设定一个固定的起步时间 (Unix 毫秒)
	// 例如：1722441600000 (2024-08-01)
	const manualStartTime int64 = 1767686400000 // 修正为毫秒，对应你之前设置的 2026 年初

	if manualStartTime > 0 {
		startTime = manualStartTime
		log.Printf("使用硬编码起始时间 (毫秒): %d", startTime)
	} else {
		result := db.DB.Model(&models.FightSyncMap{}).Where("player_id = ?", playerID).Order("timestamp desc").First(&lastSync)
		if result.Error == nil {
			startTime = lastSync.Timestamp * 1000
			log.Printf("发现历史记录，从时间戳 %d 开始增量拉取", startTime)
		} else {
			log.Println("未发现历史记录，将拉取全量数据")
		}
	}

	// 2. 构造 GraphQL 查询（分页 recentReports），向前拉取至截止日期（默认近 90 天）
	serverSlug := server
	limit := 20
	cutoff := time.Now().AddDate(0, -3, 0).UnixMilli()

	query := `
	query ($name: String, $server: String, $region: String, $limit: Int, $page: Int) {
		rateLimitData {
			limitPerHour
			pointsSpentThisHour
			pointsResetIn
		}
		characterData {
			character(name: $name, serverSlug: $server, serverRegion: $region) {
				id
				name
				recentReports(limit: $limit, page: $page) {
					last_page
					data {
						code
						title
						startTime
						endTime
						masterData {
							logVersion
							gameVersion
							lang
							actors {
								id
								name
								subType
								gameID
								type
							}       
						}
					fights(difficulty: 101) {
						id
						name
						kill
						friendlyPlayers
						startTime
						endTime
						fightPercentage
						bossPercentage
						encounterID
						difficulty
						gameZone{
							id
							name
						} 
					}
					}
				}
			}
		}
	}`

	pageConcurrency := getPageConcurrency()

	log.Printf("[DEBUG] Querying FFLogs: Name=%s, ServerSlug=%s, Region=%s, Page=%d, Limit=%d, Cutoff(ms)=%d", name, serverSlug, region, 1, limit, cutoff)
	firstVars := map[string]interface{}{
		"name":   name,
		"server": serverSlug,
		"region": region,
		"limit":  limit,
		"page":   1,
	}
	// 3. 处理第一页数据，获取总页数和最早报告时间
	firstData, err := s.client.ExecuteQuery(ctx, query, firstVars)
	if err != nil {
		return fmt.Errorf("同步失败: %v", err)
	}
	// 4. 如果未达到截止日期且有多页，继续并发拉取后续页面，直到达到截止日期或拉取完所有页面
	reports, lastPage, err := extractReportsFromData(firstData)
	if err != nil {
		return err
	}
	// 获取当前批次中最早的报告时间（毫秒），用于分页终止条件
	oldest, count := getOldestReportTimeFromReports(reports)
	if count == 0 {
		log.Printf("[INFO] 未发现报告数据，结束同步")
		if err := s.scorePendingDownloadedFights(ctx, playerID); err != nil {
			log.Printf("[SCORE] pending score pass failed on empty report set: %v", err)
		}
		if err := markCompletedReportParseLogs(playerID); err != nil {
			return err
		}
		if err := s.refreshPlayerOutputAbilityFromLogs(ctx, playerID, name, serverSlug, region); err != nil {
			return err
		}
		return touchPlayerV2ReportCheckedAt(playerID)
	}

	allReports := append([]interface{}{}, reports...)
	stop := atomic.Bool{}
	if oldest <= cutoff {
		stop.Store(true)
		log.Printf("[INFO] 已达到截止日期，最早报告时间: %d (ms)", oldest)
	}

	if !stop.Load() && lastPage > 1 {
		log.Printf("[INFO] 最近报告分页总数: %d, 并发: %d", lastPage, pageConcurrency)

		type pageResult struct {
			page    int
			reports []interface{}
			oldest  int64
			count   int
			err     error
		}
		// 使用 channel 和 WaitGroup 来协调分页查询的并发执行和结果收集
		pages := make(chan int)
		// results 频道用于收集每个分页查询的结果，包括报告列表、最早报告时间和可能的错误
		results := make(chan pageResult)
		var wg sync.WaitGroup
		//这是一个生产者-消费者模式，pages 频道由一个 goroutine 负责发送页码，多个 worker goroutine 负责消费页码并查询数据，最后一个 goroutine 等待所有 worker 完成后关闭结果频道。
		for i := 0; i < pageConcurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for page := range pages {
					if stop.Load() {
						continue
					}

					log.Printf("[DEBUG] Querying FFLogs: Name=%s, ServerSlug=%s, Region=%s, Page=%d, Limit=%d, Cutoff(ms)=%d", name, serverSlug, region, page, limit, cutoff)
					vars := map[string]interface{}{
						"name":   name,
						"server": serverSlug,
						"region": region,
						"limit":  limit,
						"page":   page,
					}

					data, err := s.client.ExecuteQuery(ctx, query, vars)
					if err != nil {
						stop.Store(true)
						results <- pageResult{page: page, err: fmt.Errorf("同步失败: %v", err)}
						continue
					}
					//这里是每个 worker goroutine 处理分页查询的结果，提取报告列表和最早报告时间，并根据截止日期判断是否继续拉取后续页面。
					pageReports, _, err := extractReportsFromData(data)
					if err != nil {
						stop.Store(true)
						results <- pageResult{page: page, err: err}
						continue
					}
					// 获取当前批次中最早的报告时间（毫秒），用于分页终止条件
					pageOldest, pageCount := getOldestReportTimeFromReports(pageReports)
					if pageCount == 0 || pageOldest <= cutoff {
						stop.Store(true)
					}

					results <- pageResult{
						page:    page,
						reports: pageReports,
						oldest:  pageOldest,
						count:   pageCount,
					}
				}
			}()
		}
		// 发送页码到 pages 频道，触发 worker goroutine 开始查询
		go func() {
			for page := 2; page <= lastPage; page++ {
				if stop.Load() {
					break
				}
				pages <- page
			}
			close(pages)
		}()

		go func() {
			wg.Wait()
			close(results)
		}()

		var fetchErr error
		// 收集 worker goroutine 的结果，汇总报告列表，并根据最早报告时间判断是否继续拉取后续页面。
		for res := range results {
			if res.err != nil && fetchErr == nil {
				fetchErr = res.err
				continue
			}
			if res.count == 0 {
				continue
			}
			if res.oldest <= cutoff {
				log.Printf("[INFO] 已达到截止日期，最早报告时间: %d (ms)", res.oldest)
			}
			allReports = append(allReports, res.reports...)
		}

		if fetchErr != nil {
			return fetchErr
		}
	}

	log.Printf("[INFO] 汇总报告数量: %d", len(allReports))
	// 5. 处理报告列表，构建战斗映射并存储 fight_sync_maps，同时记录 reports 以便后续解析和 V1 下载
	if err := s.syncAllReportsMetadata(playerID, name, allReports); err != nil {
		return err
	}
	if _, err := s.downloadV1Reports(ctx, playerID); err != nil {
		return err
	}
	if err := s.refreshPlayerOutputAbilityFromLogs(ctx, playerID, name, serverSlug, region); err != nil {
		return err
	}
	if err := touchPlayerV2ReportCheckedAt(playerID); err != nil {
		return err
	}

	return nil
}

func (s *SyncManager) selfHealReportDownloadStatus(playerID uint) error {
	var pendingFights int64
	if err := db.DB.Model(&models.FightSyncMap{}).
		Where("player_id = ? AND downloaded = ?", playerID, false).
		Count(&pendingFights).Error; err != nil {
		return fmt.Errorf("check pending fights failed: %v", err)
	}

	var pendingReports int64
	if err := db.DB.Model(&models.Report{}).
		Where("player_id = ? AND downloaded = ?", playerID, false).
		Count(&pendingReports).Error; err != nil {
		return fmt.Errorf("check pending reports failed: %v", err)
	}

	if pendingFights == 0 && pendingReports > 0 {
		log.Printf("[INFO] fights 已全下载但 reports 未更新，开始自检修复")
		if err := markCompletedReportParseLogs(playerID); err != nil {
			return fmt.Errorf("self-heal reports failed: %v", err)
		}
	}

	return nil
}

func (s *SyncManager) scorePendingDownloadedFights(ctx context.Context, playerID uint) error {
	if s.scorer == nil {
		return nil
	}

	var maps []models.FightSyncMap
	if err := db.DB.Select("id", "master_id").
		Where("player_id = ? AND downloaded = ? AND (parsed_done = ? OR scored_at IS NULL OR scored_at < ?)", playerID, true, false, time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)).
		Find(&maps).Error; err != nil {
		return fmt.Errorf("load pending scoring fights failed: %v", err)
	}
	if len(maps) == 0 {
		return nil
	}

	type scoreTask struct {
		mappingID  uint
		reportCode string
		fightID    int
	}

	tasks := make([]scoreTask, 0, len(maps))
	for _, mapping := range maps {
		code, fightID, ok := splitMasterID(mapping.MasterID)
		if !ok {
			log.Printf("[SCORE] skip invalid master_id: %s", mapping.MasterID)
			continue
		}
		tasks = append(tasks, scoreTask{mappingID: mapping.ID, reportCode: code, fightID: fightID})
	}
	if len(tasks) == 0 {
		return nil
	}

	recommendedWorkers := s.scorer.RecommendedWorkerCount()
	workerCount := getScoreConcurrency(s.scorer)
	if workerCount <= 0 {
		workerCount = 1
	}
	if recommendedWorkers > 0 {
		if workerCount == recommendedWorkers {
			log.Printf("[SCORE] 本机推荐解析并发=%d（已采用）", recommendedWorkers)
		} else {
			log.Printf("[SCORE] 本机推荐解析并发=%d，当前并发=%d（可用 FFLOGS_SCORE_CONCURRENCY 覆盖）", recommendedWorkers, workerCount)
		}
	}
	totalCount := int64(len(tasks))
	startedAt := time.Now()
	var parsedCount int64
	var activeCount int64

	logProgress := func(reportCode string, fightID int, status string, analyzeHost string) {
		parsed := atomic.LoadInt64(&parsedCount)
		active := atomic.LoadInt64(&activeCount)

		elapsedSec := time.Since(startedAt).Seconds()
		speed := 0.0
		if elapsedSec > 0 {
			speed = float64(parsed) / elapsedSec
		}

		eta := "--"
		remaining := totalCount - parsed
		if remaining <= 0 {
			eta = "完成"
		} else if speed > 0 {
			etaAt := time.Now().Add(time.Duration(float64(remaining)/speed) * time.Second)
			eta = etaAt.Format("15:04:05")
		}

		hostLabel := strings.TrimSpace(analyzeHost)
		if hostLabel == "" {
			hostLabel = "未知"
		}

		log.Printf("[SCORE] %s-%d %s | 已解析=%d/%d 当前并发=%d/%d 当前速度=%.2f fights/s 预计完成=%s 解析主机=%s",
			reportCode, fightID, status, parsed, totalCount, active, int64(workerCount), speed, eta, hostLabel)
	}

	log.Printf("[SCORE] 解析进度启动 | 已解析=0/%d 当前并发=0/%d 当前速度=0.00 fights/s 预计完成=--", totalCount, workerCount)
	sem := make(chan struct{}, workerCount)
	var wg sync.WaitGroup

	for _, task := range tasks {
		wg.Add(1)
		go func(mappingID uint, reportCode string, fightID int) {
			defer wg.Done()
			sem <- struct{}{}
			atomic.AddInt64(&activeCount, 1)
			defer func() {
				atomic.AddInt64(&activeCount, -1)
				<-sem
			}()

			scoreCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()

			analyzeHost, err := s.scorer.ScoreFightWithEndpoint(scoreCtx, playerID, reportCode, fightID)
			if err != nil {
				if scoring.IsNoMatchedActorError(err) {
					if markErr := markFightSkippedNoMatch(mappingID); markErr != nil {
						log.Printf("[SCORE] fight %s-%d skip-mark failed: %v", reportCode, fightID, markErr)
						logProgress(reportCode, fightID, "失败(标记跳过)", analyzeHost)
						return
					}
					atomic.AddInt64(&parsedCount, 1)
					logProgress(reportCode, fightID, "跳过(无匹配角色)", analyzeHost)
					return
				}
				log.Printf("[SCORE] fight %s-%d failed: %v", reportCode, fightID, err)
				logProgress(reportCode, fightID, "失败(评分)", analyzeHost)
				return
			}

			updated, markErr := markFightParsedDoneIdempotent(mappingID)
			if markErr != nil {
				log.Printf("[SCORE] mark parsed_done failed (%s-%d): %v", reportCode, fightID, markErr)
				logProgress(reportCode, fightID, "失败(更新parsed_done)", analyzeHost)
				return
			}
			atomic.AddInt64(&parsedCount, 1)
			if updated {
				logProgress(reportCode, fightID, "完成", analyzeHost)
			} else {
				logProgress(reportCode, fightID, "完成(幂等跳过)", analyzeHost)
			}
		}(task.mappingID, task.reportCode, task.fightID)
	}

	wg.Wait()
	finalParsed := atomic.LoadInt64(&parsedCount)
	if finalParsed == totalCount {
		log.Printf("[SCORE] 解析完成 | 已解析=%d/%d", finalParsed, totalCount)
	} else {
		log.Printf("[SCORE] 解析结束(部分失败) | 已解析=%d/%d", finalParsed, totalCount)
	}
	return nil
}

func getScoreConcurrency(scorer *scoring.Service) int {
	const defaultConcurrency = 2
	if raw := strings.TrimSpace(os.Getenv("FFLOGS_SCORE_CONCURRENCY")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}

	if scorer != nil {
		if v := scorer.RecommendedWorkerCount(); v > 0 {
			return v
		}
	}

	parsePositiveInt := func(key string) int {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return 0
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			return 0
		}
		return v
	}

	threadWorkers := parsePositiveInt("XIVA_THREAD_POOL_SIZE")
	if threadWorkers == 0 {
		threadWorkers = parsePositiveInt("XIVA_PORT_COUNT")
	}
	perWorker := parsePositiveInt("XIVA_CALL_CONCURRENCY")
	if perWorker == 0 {
		perWorker = 1
	}
	if threadWorkers > 0 {
		return threadWorkers * perWorker
	}

	if v := parsePositiveInt("XIVA_CALL_CONCURRENCY"); v > 0 {
		return v
	}
	return defaultConcurrency
}

func markFightParsedDoneIdempotent(mappingID uint) (bool, error) {
	res := db.DB.Model(&models.FightSyncMap{}).
		Where("id = ? AND parsed_done = ?", mappingID, false).
		Update("parsed_done", true)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func markFightSkippedNoMatch(mappingID uint) error {
	var row models.FightSyncMap
	if err := db.DB.Select("job").Where("id = ?", mappingID).First(&row).Error; err != nil {
		return err
	}

	normalizedJob := scoring.CanonicalJobKey(row.Job)
	if normalizedJob == "" {
		normalizedJob = strings.ToUpper(strings.TrimSpace(row.Job))
	}

	return db.DB.Model(&models.FightSyncMap{}).
		Where("id = ?", mappingID).
		Updates(map[string]interface{}{
			"job":                   normalizedJob,
			"parsed_done":           true,
			"scored_at":             time.Now(),
			"score_actor_name":      "SKIPPED_NO_MATCH",
			"checklist_abs":         0,
			"checklist_confidence":  0,
			"checklist_adj":         0,
			"suggestion_penalty":    0,
			"utility_score":         0,
			"survival_penalty":      0,
			"job_module_score":      0,
			"battle_score":          0,
			"fight_weight":          0,
			"weighted_battle_score": 0,
		}).Error
}

func (s *SyncManager) processSyncData(playerID uint, charName string, data map[string]interface{}) error {
	reports, _, err := extractReportsFromData(data)
	if err != nil {
		log.Printf("[DEBUG] GraphQL Response: %+v", data)
		return err
	}

	return s.processSyncReports(playerID, charName, reports)
}

// processSyncReports 处理报告列表数据，构建战斗映射并存储 fight_sync_maps，同时记录 reports 以便后续解析和 V1 下载
func (s *SyncManager) processSyncReports(playerID uint, charName string, reports []interface{}) error {
	if len(reports) == 0 {
		return nil
	}

	reportMetadata := make(map[string]datatypes.JSON)
	reportSummaries := make(map[string]reportSummary)
	for _, r := range reports {
		reportMap, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		reportCode, ok := reportMap["code"].(string)
		if !ok || reportCode == "" {
			continue
		}
		if _, exists := reportMetadata[reportCode]; exists {
			continue
		}
		metaJSON, err := buildReportMetadata(reportMap)
		if err != nil {
			log.Printf("[WARN] build report metadata failed for %s: %v", reportCode, err)
			continue
		}
		reportMetadata[reportCode] = metaJSON

		if summary, ok := buildReportSummary(reportMap); ok {
			reportSummaries[reportCode] = summary
		}
	}

	uniqueCodes := uniqueReportCodes(reports)
	knownCodes := make(map[string]struct{}, len(uniqueCodes))
	if len(uniqueCodes) > 0 {
		var existingLogs []models.Report
		if err := db.DB.
			Where("player_id = ? AND source_report IN ?", playerID, uniqueCodes).
			Find(&existingLogs).Error; err != nil {
			return fmt.Errorf("load reports failed: %v", err)
		}
		for _, logEntry := range existingLogs {
			if logEntry.SourceReport != "" {
				knownCodes[logEntry.SourceReport] = struct{}{}
			}
		}
	}

	// 先构建内存去重映射，再写入 fight_sync_maps
	var reportInfos []*reportInfo
	for _, r := range reports {
		reportMap := r.(map[string]interface{})
		reportCode := reportMap["code"].(string)
		if _, exists := knownCodes[reportCode]; exists {
			continue
		}
		reportStart := int64(reportMap["startTime"].(float64))
		fightsRaw, ok := reportMap["fights"]
		if !ok || fightsRaw == nil {
			log.Printf("[INFO] 报告 %s fights 为空，跳过", reportCode)
			continue
		}
		fights, ok := fightsRaw.([]interface{})
		if !ok || len(fights) == 0 {
			log.Printf("[INFO] 报告 %s fights 为空，跳过", reportCode)
			continue
		}

		playerJob, actorID := s.resolveActorInfo(reportMap, charName)
		if actorID == 0 {
			continue
		}
		actorNameByID := resolveActorNameMap(reportMap)

		filtered := make([]fightInfo, 0)
		for _, f := range fights {
			fight := f.(map[string]interface{})
			fightID := int(fight["id"].(float64))

			difficultyVal := fight["difficulty"]
			if difficultyVal == nil {
				continue
			}
			diff := int(difficultyVal.(float64))
			if diff != 101 {
				continue
			}

			if !s.isActorInFight(fight, actorID) {
				continue
			}

			fStart, ok1 := fight["startTime"].(float64)
			fEnd, ok2 := fight["endTime"].(float64)
			if !ok1 || !ok2 {
				continue
			}
			duration := int((fEnd - fStart) / 1000)
			fightTimestamp := reportStart/1000 + int64(fStart/1000)
			reportFightID := fmt.Sprintf("%s-%d", reportCode, fightID)

			fightName := fight["name"].(string)
			floor, isTarget := mapFloor(fightName)
			if !isTarget {
				continue
			}

			filtered = append(filtered, fightInfo{
				fight:          fight,
				reportCode:     reportCode,
				fightID:        fightID,
				fightTimestamp: fightTimestamp,
				duration:       duration,
				reportFightID:  reportFightID,
				name:           fightName,
				floor:          floor,
				fStart:         int64(fStart),
				fEnd:           int64(fEnd),
				actorID:        actorID,
				playerJob:      playerJob,
				friendPlayers:  resolveFriendlyPlayerNames(fight, actorNameByID),
			})
		}

		if len(filtered) == 0 {
			continue
		}

		reportInfos = append(reportInfos, &reportInfo{
			code:       reportCode,
			actorID:    actorID,
			playerJob:  playerJob,
			fightCount: len(filtered),
			fights:     filtered,
		})
	}
	if len(reportInfos) == 0 {
		log.Printf("[INFO] 未发现符合条件的战斗（难度101且boss映射命中）")
		return nil
	}

	masters := buildMasterFights(reportInfos)
	if len(masters) == 0 {
		log.Printf("[INFO] 本批次未生成战斗映射")
		return nil
	}
	mastersByReport := groupMastersByReport(masters)
	if err := s.writeReports(playerID, masters, reportMetadata, reportSummaries); err != nil {
		return err
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for _, group := range mastersByReport {
		wg.Add(1)
		go func(masterGroup []*masterFight) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			reportCode := masterGroup[0].reportCode
			actorID := masterGroup[0].actorID
			playerJob := masterGroup[0].playerJob
			if actorID == 0 {
				return
			}

			batch := make([]models.FightSyncMap, 0, len(masterGroup))
			for _, master := range masterGroup {
				fight := master.fight
				bossPercentage := 0.0
				if bp, ok := fight["bossPercentage"].(float64); ok {
					bossPercentage = bp
				}
				fightPercentage := 0.0
				if fp, ok := fight["fightPercentage"].(float64); ok {
					fightPercentage = fp
				}
				difficulty := 0
				if dv, ok := fight["difficulty"].(float64); ok {
					difficulty = int(dv)
				}
				encounterID := 0
				if ev, ok := fight["encounterID"].(float64); ok {
					encounterID = int(ev)
				}
				var gameZoneJSON datatypes.JSON
				if gz, ok := fight["gameZone"].(map[string]interface{}); ok {
					if raw, err := json.Marshal(gz); err == nil {
						gameZoneJSON = datatypes.JSON(raw)
					}
				}

				// eventsRaw, errEvents := s.fetchFightEvents(reportCode, master.fightID, master.fStart, master.fEnd)
				// if errEvents != nil {
				// 	log.Printf("[WARN] 拉取事件失败，略过 events 缓存: %v", errEvents)
				// }

				// combinedRaw := rawData
				// if len(eventsRaw) > 0 {
				// 	combinedRaw = mergeEventPayload(rawData, eventsRaw)
				// }

				// s.saveFightCache(master.reportFightID, master.fightTimestamp, int(master.fEnd-master.fStart), deaths, vulns, avoidable, percentile, combinedRaw)

				batch = append(batch, models.FightSyncMap{
					MasterID:            master.reportFightID,
					SourceIDs:           master.sourceIDs,
					FriendPlayers:       master.friendPlayers,
					FriendPlayersUsable: isFriendPlayersUsable(master.friendPlayers),
					PlayerID:            playerID,
					Timestamp:           master.fightTimestamp,
					FightID:             master.fightID,
					Kill:                fight["kill"].(bool),
					Job:                 playerJob,
					Downloaded:          false,
					StartTime:           master.fStart,
					EndTime:             master.fEnd,
					Name:                master.name,
					BossPercentage:      bossPercentage,
					FightPercentage:     fightPercentage,
					Floor:               master.floor,
					GameZone:            gameZoneJSON,
					Difficulty:          difficulty,
					EncounterID:         encounterID,
				})
			}

			if err := s.batchUpsertFightSyncMaps(batch); err != nil {
				log.Printf("批量写入报告 %s 失败: %v", reportCode, err)
			}
		}(group)
	}

	wg.Wait()
	log.Printf("多线程并行同步完成")
	return nil
}

// mergeEventPayload 将战斗详情数据和事件数据合并成一个新的 JSON 数据，方便后续存储和使用
func (s *SyncManager) writeReports(playerID uint, masters []*masterFight, reportMetadata map[string]datatypes.JSON, reportSummaries map[string]reportSummary) error {
	if len(masters) == 0 {
		return nil
	}
	entries := make(map[string]map[string]struct{})
	for _, master := range masters {
		if master == nil {
			continue
		}
		masterCode := master.reportCode
		if masterCode == "" {
			continue
		}
		set, ok := entries[masterCode]
		if !ok {
			set = make(map[string]struct{})
			entries[masterCode] = set
		}
		// Ensure a canonical self-row exists for each master report.
		set[masterCode] = struct{}{}
		for _, sourceID := range master.sourceIDs {
			sourceCode, ok := splitReportCode(sourceID)
			if !ok || sourceCode == "" {
				continue
			}
			set[sourceCode] = struct{}{}
		}
	}
	if len(entries) == 0 {
		return nil
	}

	// now := time.Now()
	for masterCode, sources := range entries {
		for sourceCode := range sources {
			logEntry := models.Report{
				MasterReport: masterCode,
				SourceReport: sourceCode,
				PlayerID:     playerID,
			}
			if err := db.DB.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "player_id"}, {Name: "source_report"}},
				DoUpdates: clause.AssignmentColumns([]string{"master_report"}),
			}).Create(&logEntry).Error; err != nil {
				return fmt.Errorf("upsert reports failed: %v", err)
			}
		}
		if meta, ok := reportMetadata[masterCode]; ok {
			if err := db.DB.Model(&models.Report{}).
				Where("player_id = ? AND master_report = ?", playerID, masterCode).
				Update("report_metadata", meta).Error; err != nil {
				return fmt.Errorf("update reports metadata failed: %v", err)
			}
		}
		if summary, ok := reportSummaries[masterCode]; ok {
			if err := db.DB.Model(&models.Report{}).
				Where("player_id = ? AND master_report = ?", playerID, masterCode).
				Updates(map[string]interface{}{
					"title":      summary.Title,
					"start_time": summary.StartTime,
					"end_time":   summary.EndTime,
				}).Error; err != nil {
				return fmt.Errorf("update reports summary failed: %v", err)
			}
		}
	}

	return nil
}

type reportSummary struct {
	Title     string
	StartTime int64
	EndTime   int64
}

func buildReportSummary(reportMap map[string]interface{}) (reportSummary, bool) {
	code, ok := reportMap["code"].(string)
	if !ok || code == "" {
		return reportSummary{}, false
	}
	start, ok := reportMap["startTime"].(float64)
	if !ok {
		return reportSummary{}, false
	}
	var title string
	if t, ok := reportMap["title"].(string); ok {
		title = t
	}

	end := int64(start)
	if fightsRaw, ok := reportMap["fights"].([]interface{}); ok {
		maxEnd := int64(0)
		for _, f := range fightsRaw {
			fight, ok := f.(map[string]interface{})
			if !ok {
				continue
			}
			if e, ok := fight["endTime"].(float64); ok {
				if int64(e) > maxEnd {
					maxEnd = int64(e)
				}
			}
		}
		if maxEnd > 0 {
			end = int64(start) + maxEnd
		}
	}

	return reportSummary{Title: title, StartTime: int64(start), EndTime: end}, true
}

func buildReportMetadata(reportMap map[string]interface{}) (datatypes.JSON, error) {
	meta := map[string]interface{}{}
	if masterData, ok := reportMap["masterData"]; ok {
		meta["masterData"] = masterData
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(data), nil
}

func getEncounterIDFilter() (map[int]struct{}, bool) {
	raw := strings.TrimSpace(os.Getenv("FFLOGS_ENCOUNTER_ID"))
	if raw == "" {
		return nil, false
	}

	trimmed := strings.Trim(raw, "[]")
	parts := strings.Split(trimmed, ",")
	if len(parts) == 1 {
		if strings.TrimSpace(parts[0]) == "" {
			return nil, false
		}
	}

	ids := make(map[int]struct{})
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		val, err := strconv.Atoi(part)
		if err != nil || val <= 0 {
			continue
		}
		ids[val] = struct{}{}
	}

	if len(ids) == 0 {
		log.Printf("[WARN] FFLOGS_ENCOUNTER_ID 无效: %s", raw)
		return nil, false
	}
	return ids, true
}

// extractReportsFromData 从 GraphQL 响应数据中提取报告列表和总页数
func extractReportsFromData(data map[string]interface{}) ([]interface{}, int, error) {
	charData, ok := data["characterData"].(map[string]interface{})
	if !ok || charData["character"] == nil {
		return nil, 0, fmt.Errorf("未找到玩家数据")
	}

	character := charData["character"].(map[string]interface{})

	var reportsData map[string]interface{}
	if rd, ok := character["reports"].(map[string]interface{}); ok {
		reportsData = rd
	} else if rd, ok := character["recentReports"].(map[string]interface{}); ok {
		reportsData = rd
	} else {
		return nil, 0, fmt.Errorf("未找到报告列表")
	}

	lastPage := 0
	if lp, ok := reportsData["last_page"].(float64); ok {
		lastPage = int(lp)
	}

	dataBlock, ok := reportsData["data"].([]interface{})
	if !ok {
		return nil, lastPage, fmt.Errorf("未找到报告数据内容")
	}
	encounterIDFilter, hasEncounterIDFilter := getEncounterIDFilter()
	filtered := make([]interface{}, 0, len(dataBlock))
	// 调试日志：输出每个报告的战斗数量，帮助确认数据结构和分页逻辑是否正确
	for _, r := range dataBlock {
		// 每个报告应该是一个 map[string]interface{}，包含 fights 字段
		reportMap, ok := r.(map[string]interface{})
		if !ok {
			// log.Printf("[DEBUG] Report %d: invalid report format", index)
			continue
		}
		// 这里假设 fights 是一个数组，如果不是，日志会指出格式问题
		fightsRaw, exists := reportMap["fights"]
		if !exists || fightsRaw == nil {
			// log.Printf("[DEBUG] Report %d: fights count = 0", index)
			continue
		}

		fights, ok := fightsRaw.([]interface{})
		if !ok {
			// log.Printf("[DEBUG] Report %d: fights format invalid", index)
			continue
		}
		if hasEncounterIDFilter {
			filteredFights := make([]interface{}, 0, len(fights))
			for _, fightRaw := range fights {
				fight, ok := fightRaw.(map[string]interface{})
				if !ok {
					continue
				}
				encVal, ok := fight["encounterID"].(float64)
				if !ok {
					continue
				}
				if _, exists := encounterIDFilter[int(encVal)]; !exists {
					continue
				}
				filteredFights = append(filteredFights, fightRaw)
			}
			fights = filteredFights
			reportMap["fights"] = fights
		}
		// 这里是每个报告的战斗数量日志，如果为0，说明该报告没有战斗数据，可能是空报告或数据结构不符合预期。
		if len(fights) == 0 {
			// log.Printf("[DEBUG] Report %d: fights count = 0", index)
			continue
		}
		// log.Printf("[DEBUG] Report %d: fights count = %d", index, len(fights))
		filtered = append(filtered, r)
	}

	return filtered, lastPage, nil
}

// 获取当前批次中最早的报告时间（毫秒），用于分页终止条件
func getOldestReportTimeFromReports(reports []interface{}) (oldestMs int64, count int) {
	oldestMs = 1<<63 - 1
	count = len(reports)
	for _, r := range reports {
		reportMap := r.(map[string]interface{})
		if st, ok := reportMap["startTime"].(float64); ok {
			ms := int64(st)
			if ms < oldestMs {
				oldestMs = ms
			}
		}
	}
	if oldestMs == 1<<63-1 {
		return 0, count
	}
	return oldestMs, count
}

func getPageConcurrency() int {
	const defaultConcurrency = 5
	if raw := os.Getenv("FFLOGS_SYNC_PAGE_CONCURRENCY"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return defaultConcurrency
}

// syncAllReportsMetadata 同步所有报告元数据并写入 reports/fight_sync_maps。
func (s *SyncManager) syncAllReportsMetadata(playerID uint, name string, reports []interface{}) error {
	if len(reports) == 0 {
		return nil
	}
	//这里的逻辑是先处理报告列表，提取报告代码，并与数据库中已有的报告解析日志进行对比，确定哪些报告是新的需要下载的 V1 报告，同时更新数据库中的报告解析日志状态，以便后续下载和处理。
	if err := s.processSyncReports(playerID, name, reports); err != nil {
		return err
	}

	// if err := os.MkdirAll(getAllReportsDir(), 0755); err != nil {
	// 	return "", nil, fmt.Errorf("create allreports dir failed: %v", err)
	// }
	// writePath := filepath.Join(getAllReportsDir(), "reports_snapshot.json")
	// if err := writeJSON(writePath, reports); err != nil {
	// 	return "", nil, fmt.Errorf("write reports snapshot failed: %v", err)
	// }

	return nil
}

// finalizeAllReportsDownloads 将已完成下载的报告标记到 reports，并更新玩家 all_report_codes。
func (s *SyncManager) finalizeAllReportsDownloads(playerID uint, downloaded []string) error {
	if len(downloaded) == 0 {
		return nil
	}

	var player models.Player
	if err := db.DB.Clauses(dbresolver.Write).Where("id = ?", playerID).First(&player).Error; err != nil {
		return fmt.Errorf("load player failed: %v", err)
	}

	merged := mergeStringSets(player.AllReportCodes, downloaded)
	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("marshal all_report_codes failed: %v", err)
	}
	if err := db.DB.Model(&player).Update("all_report_codes", datatypes.JSON(mergedJSON)).Error; err != nil {
		return fmt.Errorf("update all_report_codes failed: %v", err)
	}
	if err := db.DB.Model(&models.Report{}).
		Where("player_id = ? AND master_report IN ?", playerID, downloaded).
		Updates(map[string]interface{}{"downloaded": true}).Error; err != nil {
		return fmt.Errorf("update reports downloaded failed: %v", err)
	}

	return nil
}

func mergeStringSets(base []string, add []string) []string {
	set := make(map[string]struct{}, len(base)+len(add))
	for _, v := range base {
		set[v] = struct{}{}
	}
	for _, v := range add {
		set[v] = struct{}{}
	}
	merged := make([]string, 0, len(set))
	for v := range set {
		merged = append(merged, v)
	}
	return merged
}

func splitReportCode(sourceID string) (string, bool) {
	idx := strings.LastIndex(sourceID, "-")
	if idx <= 0 {
		return "", false
	}
	return sourceID[:idx], true
}

func (s *SyncManager) batchUpsertFightSyncMaps(maps []models.FightSyncMap) error {
	if len(maps) == 0 {
		return nil
	}
	return db.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "name"}, {Name: "timestamp"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"source_ids": gorm.Expr(
				"(SELECT jsonb_agg(DISTINCT value) FROM (" +
					"SELECT jsonb_array_elements(COALESCE(fight_sync_maps.source_ids, '[]'::jsonb)) AS value " +
					"UNION SELECT jsonb_array_elements(COALESCE(EXCLUDED.source_ids, '[]'::jsonb)) AS value" +
					") AS merged)",
			),
			"friendplayers": gorm.Expr(
				"CASE " +
					"WHEN jsonb_array_length(COALESCE(EXCLUDED.friendplayers, '[]'::jsonb)) >= jsonb_array_length(COALESCE(fight_sync_maps.friendplayers, '[]'::jsonb)) " +
					"THEN COALESCE(EXCLUDED.friendplayers, '[]'::jsonb) " +
					"ELSE COALESCE(fight_sync_maps.friendplayers, '[]'::jsonb) " +
					"END",
			),
			"friendplayers_usable": gorm.Expr("COALESCE(fight_sync_maps.friendplayers_usable, false) OR COALESCE(EXCLUDED.friendplayers_usable, false)"),
		}),
	}).CreateInBatches(maps, 200).Error
}

// uniqueReportCodes 从报告列表中提取唯一的报告代码，返回一个字符串切片
func uniqueReportCodes(reports []interface{}) []string {
	set := make(map[string]struct{})
	for _, r := range reports {
		reportMap, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		code, ok := reportMap["code"].(string)
		if !ok || code == "" {
			continue
		}
		set[code] = struct{}{}
	}

	out := make([]string, 0, len(set))
	for code := range set {
		out = append(out, code)
	}
	return out
}

// 辅助函数：检查 Actor 是否在某场战斗中
func (s *SyncManager) isActorInFight(fight map[string]interface{}, actorID int) bool {
	for _, fid := range parseFriendlyPlayerIDs(fight["friendlyPlayers"]) {
		if fid == actorID {
			return true
		}
	}
	return false
}

func resolveActorNameMap(reportMap map[string]interface{}) map[int]string {
	out := make(map[int]string)
	md, ok := reportMap["masterData"].(map[string]interface{})
	if !ok {
		return out
	}
	actors, ok := md["actors"].([]interface{})
	if !ok {
		return out
	}
	for _, actorAny := range actors {
		actor, ok := actorAny.(map[string]interface{})
		if !ok {
			continue
		}
		actorType, _ := actor["type"].(string)
		if actorType != "" && !strings.EqualFold(strings.TrimSpace(actorType), "player") {
			continue
		}
		idRaw, ok := actor["id"].(float64)
		if !ok {
			continue
		}
		name, _ := actor["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if shouldExcludeFriendlyPlayerName(name) {
			continue
		}
		out[int(idRaw)] = name
	}
	return out
}

func resolveFriendlyPlayerNames(fight map[string]interface{}, actorNameByID map[int]string) []string {
	ids := parseFriendlyPlayerIDs(fight["friendlyPlayers"])
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		name := strings.TrimSpace(actorNameByID[id])
		if name == "" {
			continue
		}
		if shouldExcludeFriendlyPlayerName(name) {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func shouldExcludeFriendlyPlayerName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == "multiple players" || n == "limit break" || n == "limitbreak"
}

func isFriendPlayersUsable(names []string) bool {
	// 8人本要求完整队伍名单（含自己）才用于开荒速度计算。
	return len(names) == 8
}

func parseFriendlyPlayerIDs(v interface{}) []int {
	friends, ok := v.([]interface{})
	if !ok || len(friends) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(friends))
	out := make([]int, 0, len(friends))
	for _, fid := range friends {
		id, ok := parseFriendlyPlayerID(fid)
		if !ok {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func parseFriendlyPlayerID(v interface{}) (int, bool) {
	switch val := v.(type) {
	case float64:
		return int(val), true
	case int:
		return val, true
	case int64:
		return int(val), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// containsString checks if a slice contains a string; avoids repeated linear scans inline.
func containsString(arr []string, target string) bool {
	for _, v := range arr {
		if v == target {
			return true
		}
	}
	return false
}

type reportInfo struct {
	code       string
	actorID    int
	playerJob  string
	fightCount int
	fights     []fightInfo
}

type fightInfo struct {
	fight          map[string]interface{}
	reportCode     string
	fightID        int
	fightTimestamp int64
	duration       int
	reportFightID  string
	name           string
	floor          string
	fStart         int64
	fEnd           int64
	actorID        int
	playerJob      string
	friendPlayers  []string
}

type masterFight struct {
	fightInfo
	reportFightCount int
	sourceIDs        []string
}

func (s *SyncManager) resolveActorInfo(reportMap map[string]interface{}, charName string) (string, int) {
	var playerJob string
	var actorID int

	if md, ok := reportMap["masterData"].(map[string]interface{}); ok {
		if actors, ok := md["actors"].([]interface{}); ok {
			for _, a := range actors {
				actor := a.(map[string]interface{})
				aName := actor["name"].(string)
				if aName == charName {
					playerJob = actor["subType"].(string)
					actorID = int(actor["id"].(float64))
				}
			}
		}
	}

	return playerJob, actorID
}

func mapFloor(rawName string) (string, bool) {
	bossMapping := map[string]string{
		"Vamp Fatale":         "M9S",
		"Red Hot / Deep Blue": "M10S",
		"The Tyrant":          "M11S",
		"Lindwurm":            "M12S",
		"Lindwurm II":         "M12S2",
	}

	mappedName, isTarget := bossMapping[rawName]
	return mappedName, isTarget
}

func buildMasterFights(reports []*reportInfo) []*masterFight {
	masters := make([]*masterFight, 0)
	for _, r := range reports {
		for _, fight := range r.fights {
			index := findMatchingMaster(masters, fight)
			if index < 0 {
				masters = append(masters, &masterFight{
					fightInfo:        fight,
					reportFightCount: r.fightCount,
					sourceIDs:        []string{fight.reportFightID},
				})
				continue
			}

			master := masters[index]
			if fight.duration > master.duration {
				master.fightInfo = fight
				master.reportFightCount = r.fightCount
			}
			if !containsString(master.sourceIDs, fight.reportFightID) {
				master.sourceIDs = append(master.sourceIDs, fight.reportFightID)
			}
		}
	}

	return masters
}

func groupMastersByReport(masters []*masterFight) map[string][]*masterFight {
	grouped := make(map[string][]*masterFight)
	for _, master := range masters {
		grouped[master.reportCode] = append(grouped[master.reportCode], master)
	}
	return grouped
}

func findMatchingMaster(masters []*masterFight, fight fightInfo) int {
	for idx, master := range masters {
		if master.floor != fight.floor {
			continue
		}
		if absInt64(master.fightTimestamp-fight.fightTimestamp) > 5 {
			continue
		}
		return idx
	}
	return -1
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func mergeSourceIDs(raw []string, add []string) ([]string, bool) {
	ids := append([]string{}, raw...)
	changed := false
	for _, id := range add {
		if !containsString(ids, id) {
			ids = append(ids, id)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	return ids, true
}

func mergeEventPayload(rawData []byte, eventsRaw []byte) []byte {
	combinedRaw := rawData
	var base map[string]interface{}
	if err := json.Unmarshal(rawData, &base); err != nil || base == nil {
		base = make(map[string]interface{})
	}

	var ev map[string]interface{}
	if err := json.Unmarshal(eventsRaw, &ev); err == nil {
		if evEvents, ok := ev["events"]; ok {
			base["events"] = evEvents
		}
		if _, ok := base["start"]; !ok {
			if evStart, ok := ev["start"]; ok {
				base["start"] = evStart
			}
		}
		if _, ok := base["end"]; !ok {
			if evEnd, ok := ev["end"]; ok {
				base["end"] = evEnd
			}
		}
		if meta, ok := base["meta"].(map[string]interface{}); ok {
			if evStart, ok := ev["start"]; ok && meta["start"] == nil {
				meta["start"] = evStart
			}
			if evEnd, ok := ev["end"]; ok && meta["end"] == nil {
				meta["end"] = evEnd
			}
		}
	}

	if merged, err := json.Marshal(base); err == nil {
		combinedRaw = merged
	}

	return combinedRaw
}

func (s *SyncManager) saveFightCache(id string, timestamp int64, durationMs int, deaths, vulns, avoidable int, percentile float64, payload []byte) {
	var cache models.FightCache
	if db.DB.Where("id = ?", id).First(&cache).Error == nil {
		cache.Deaths = deaths
		cache.VulnStacks = vulns
		cache.AvoidableDamage = avoidable
		cache.Percentile = percentile
		cache.Data = payload
		cache.Timestamp = timestamp
		cache.Duration = durationMs
		_ = db.DB.Save(&cache)
		return
	}

	_ = db.DB.Create(&models.FightCache{
		ID:              id,
		Duration:        durationMs,
		Deaths:          deaths,
		VulnStacks:      vulns,
		AvoidableDamage: avoidable,
		Percentile:      percentile,
		Timestamp:       timestamp,
		Data:            payload,
	})
}

func (s *SyncManager) upsertFightSyncMap(newMap models.FightSyncMap) {
	var existing models.FightSyncMap
	errSearch := db.DB.Clauses(dbresolver.Write).Where(
		"name = ? AND ABS(timestamp - ?) <= 5",
		newMap.Name,
		newMap.Timestamp,
	).First(&existing).Error

	if errSearch == nil {
		merged, changed := mergeSourceIDs(existing.SourceIDs, newMap.SourceIDs)
		if changed {
			if errUpdate := db.DB.Model(&existing).Update("source_ids", merged).Error; errUpdate != nil {
				log.Printf("[ERROR] 更新映射表失败: %v", errUpdate)
			}
		}
		return
	}

	if errCreate := db.DB.Table("fight_sync_maps").Create(&newMap).Error; errCreate != nil {
		if strings.Contains(errCreate.Error(), "duplicate key") {
			if errRetry := db.DB.Clauses(dbresolver.Write).Where(
				"name = ? AND ABS(timestamp - ?) <= 5",
				newMap.Name,
				newMap.Timestamp,
			).First(&existing).Error; errRetry == nil {
				merged, changed := mergeSourceIDs(existing.SourceIDs, newMap.SourceIDs)
				if changed {
					if errUpdate := db.DB.Model(&existing).Update("source_ids", merged).Error; errUpdate != nil {
						log.Printf("[ERROR] 并发合并 source_ids 失败: %v", errUpdate)
					}
				}
			} else {
				log.Printf("[ERROR] 并发创建映射记录失败: %v", errCreate)
			}
		} else {
			log.Printf("[SYNC] 已创建新战斗映射: %s (Name: %s, Time: %d)", newMap.MasterID, newMap.Name, newMap.Timestamp)
		}
	}
}

// ensurePlayerID 确保玩家记录存在，返回玩家 ID；如果不存在则创建新记录
func ensurePlayerID(name, server, region string) (uint, bool, error) {
	var player models.Player
	err := db.DB.Clauses(dbresolver.Write).Where("name = ? AND server = ? AND region = ?", name, server, region).First(&player).Error
	if err == nil {
		return player.ID, false, nil
	}

	newPlayer := models.Player{
		Name:   name,
		Server: server,
		Region: region,
	}
	if errCreate := db.DB.Clauses(dbresolver.Write).Create(&newPlayer).Error; errCreate != nil {
		return 0, false, errCreate
	}

	return newPlayer.ID, true, nil
}

func shouldRunV2ReportQuery(playerID uint, newlyCreated bool) (bool, string, error) {
	if playerID == 0 {
		return true, "player_id=0 fallback", nil
	}
	if newlyCreated {
		return true, "新创建玩家，首次同步直接查询", nil
	}

	var player models.Player
	if err := db.DB.Select("id", "created_at", "updated_at").Where("id = ?", playerID).First(&player).Error; err != nil {
		return false, "", err
	}

	if player.UpdatedAt.IsZero() {
		return true, "updated_at 为空，执行查询", nil
	}

	if player.CreatedAt.Equal(player.UpdatedAt) {
		if time.Since(player.CreatedAt) < 24*time.Hour {
			return true, "新创建玩家（created_at==updated_at 且 <24h）", nil
		}
	}

	age := time.Since(player.UpdatedAt)
	if age >= 24*time.Hour {
		return true, fmt.Sprintf("updated_at 超过24小时（%.1fh）", age.Hours()), nil
	}

	return false, fmt.Sprintf("updated_at 距今 %.1fh，未超过24小时", age.Hours()), nil
}

func touchPlayerV2ReportCheckedAt(playerID uint) error {
	if playerID == 0 {
		return nil
	}
	return db.DB.Model(&models.Player{}).
		Where("id = ?", playerID).
		UpdateColumn("updated_at", time.Now()).Error
}

func (s *SyncManager) refreshPlayerOutputAbilityFromLogs(ctx context.Context, playerID uint, name, server, region string) error {
	name = strings.TrimSpace(name)
	server = strings.TrimSpace(server)
	region = strings.TrimSpace(region)
	if playerID == 0 || name == "" || server == "" || region == "" {
		return nil
	}

	ability, err := s.fetchCharacterOutputAbility(ctx, name, server, region)
	if err != nil {
		return err
	}

	return db.DB.Model(&models.Player{}).
		Where("id = ?", playerID).
		UpdateColumn("output_ability", ability).Error
}

func (s *SyncManager) fetchCharacterOutputAbility(ctx context.Context, name, server, region string) (float64, error) {
	query := `
	query ($name: String, $server: String, $region: String) {
		rateLimitData {
			limitPerHour
			pointsSpentThisHour
			pointsResetIn
		}
		characterData {
			character(name: $name, serverSlug: $server, serverRegion: $region) {
				zoneRankings(difficulty: 101, metric: rdps)
			}
		}
	}`

	data, err := s.client.ExecuteQuery(ctx, query, map[string]interface{}{
		"name":   name,
		"server": server,
		"region": region,
	})
	if err != nil {
		return 0, fmt.Errorf("fetch character logs ranking failed: %v", err)
	}

	characterData, _ := data["characterData"].(map[string]interface{})
	character, _ := characterData["character"].(map[string]interface{})
	if character == nil {
		return 0, fmt.Errorf("character not found in logs ranking response")
	}

	zoneRankingsRaw, exists := character["zoneRankings"]
	if !exists || zoneRankingsRaw == nil {
		return 0, fmt.Errorf("zoneRankings missing in logs ranking response")
	}

	ability, ok := extractOutputAbilityFromZoneRankings(zoneRankingsRaw)
	if !ok {
		return 0, fmt.Errorf("unable to parse output ability from zoneRankings")
	}

	return clampPercent(ability), nil
}

func extractOutputAbilityFromZoneRankings(raw interface{}) (float64, bool) {
	switch v := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0, false
		}
		var parsed interface{}
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			return 0, false
		}
		return extractOutputAbilityFromZoneRankings(parsed)
	case map[string]interface{}:
		if val, ok := parseRankingEntryFloat(v, "bestPerformanceAverage", "medianPerformanceAverage", "averagePerformance"); ok && val > 0 {
			return val, true
		}
		if rankings, ok := v["rankings"].([]interface{}); ok {
			if avg, ok := extractPercentileFromRankingEntries(rankings); ok {
				return avg, true
			}
		}
		if entries, ok := v["data"].([]interface{}); ok {
			if avg, ok := extractPercentileFromRankingEntries(entries); ok {
				return avg, true
			}
		}
	case []interface{}:
		if avg, ok := extractPercentileFromRankingEntries(v); ok {
			return avg, true
		}
	}
	return 0, false
}

func extractPercentileFromRankingEntries(entries []interface{}) (float64, bool) {
	if len(entries) == 0 {
		return 0, false
	}
	sum := 0.0
	count := 0.0
	for _, item := range entries {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		percentile, ok := parseRankingEntryFloat(entry, "percentile", "rankPercent", "rank_percent")
		if !ok || percentile <= 0 {
			continue
		}
		sum += percentile
		count++
	}
	if count == 0 {
		return 0, false
	}
	return sum / count, true
}

func clampPercent(v float64) float64 {
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return math.Round(v*100) / 100
}

func parseRankingEntryFloat(entry map[string]interface{}, keys ...string) (float64, bool) {
	for _, key := range keys {
		value, ok := entry[key]
		if !ok || value == nil {
			continue
		}
		switch v := value.(type) {
		case float64:
			return v, true
		case float32:
			return float64(v), true
		case int:
			return float64(v), true
		case int64:
			return float64(v), true
		case string:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

func (s *SyncManager) fetchFullReportTableData(reportCode string) (map[string]interface{}, error) {
	// 1. 首先获取报告中所有的战斗记录 ID，用于构造批量查询
	queryFights := `
	query ($code: String) {
		reportData {
			report(code: $code) {
				fights {
					id
				}
			}
		}
	}`

	fData, err := s.client.ExecuteQuery(context.Background(), queryFights, map[string]interface{}{"code": reportCode})
	if err != nil {
		return nil, err
	}

	reportData, _ := fData["reportData"].(map[string]interface{})
	report, ok := reportData["report"].(map[string]interface{})
	if !ok || report == nil {
		return nil, fmt.Errorf("report not found")
	}

	var fightIDs []int
	if fs, ok := report["fights"].([]interface{}); ok {
		for _, f := range fs {
			fight := f.(map[string]interface{})
			fightIDs = append(fightIDs, int(fight["id"].(float64)))
		}
	}

	if len(fightIDs) == 0 {
		return nil, fmt.Errorf("no fights in report")
	}

	// 2. 使用显式的 fightIDs 数组进行批量拉取详情 (规避 startTime/endTime 报错)
	// 注意：Rankings 可能在批量查询中报错，先移除它以确保稳定同步
	queryTables := `
	query ($code: String, $fids: [Int]) {
		rateLimitData {
			limitPerHour
			pointsSpentThisHour
			pointsResetIn
		}
		reportData {
			report(code: $code) {
				deaths: table(dataType: Deaths, fightIDs: $fids)
				debuffs: table(dataType: Debuffs, fightIDs: $fids)
				buffs: table(dataType: Buffs, fightIDs: $fids)
				casts: table(dataType: Casts, fightIDs: $fids)
				damageDone: table(dataType: DamageDone, fightIDs: $fids)
				healing: table(dataType: Healing, fightIDs: $fids)
				damageTaken: table(dataType: DamageTaken, fightIDs: $fids)
				avoidable: table(dataType: DamageTaken, fightIDs: $fids, filterExpression: "ability.damageIsAvoidable = true")
			}
		}
	}`

	variables := map[string]interface{}{
		"code": reportCode,
		"fids": fightIDs,
	}

	data, err := s.client.ExecuteQuery(context.Background(), queryTables, variables)
	if err != nil {
		return nil, err
	}

	resReportData, _ := data["reportData"].(map[string]interface{})
	return resReportData["report"].(map[string]interface{}), nil
}

// fetchFightEvents 拉取指定战斗的事件流（分页拉取完整事件，输出 xivanalysis 友好结构）
func (s *SyncManager) fetchFightEvents(reportCode string, fightID int, startMs int64, endMs int64) ([]byte, error) {
	const limit = 5000

	query := `
	query ($code: String, $start: Float, $end: Float, $fids: [Int], $limit: Int) {
		reportData {
			report(code: $code) {
				events(startTime: $start, endTime: $end, fightIDs: $fids, limit: $limit) {
					data
					nextPageTimestamp
				}
			}
		}
	}
	`

	startCursor := startMs
	allEvents := make([]interface{}, 0)

	for {
		vars := map[string]interface{}{
			"code":  reportCode,
			"start": startCursor,
			"end":   endMs,
			"fids":  []int{fightID},
			"limit": limit,
		}

		data, err := s.client.ExecuteQuery(context.Background(), query, vars)
		if err != nil {
			return nil, err
		}

		reportData, ok := data["reportData"].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("reportData missing in events response")
		}
		report, ok := reportData["report"].(map[string]interface{})
		if !ok || report == nil {
			return nil, fmt.Errorf("report missing in events response")
		}

		eventsBlock, ok := report["events"].(map[string]interface{})
		if !ok || eventsBlock == nil {
			return nil, fmt.Errorf("events block missing")
		}

		rows, _ := eventsBlock["data"].([]interface{})
		if len(rows) == 0 {
			break
		}
		allEvents = append(allEvents, rows...)

		nextTs, hasNext := eventsBlock["nextPageTimestamp"].(float64)
		if !hasNext {
			break
		}
		nextStart := int64(nextTs)
		if nextStart <= startCursor || nextStart >= endMs {
			break
		}
		startCursor = nextStart
	}

	if len(allEvents) == 0 {
		return nil, nil
	}

	return json.Marshal(map[string]interface{}{
		"fight_id": fightID,
		"start":    startMs,
		"end":      endMs,
		"events":   allEvents,
	})
}

// extractFightDataFromReportV2 提取更完整的表数据并附带时间戳元信息
func (s *SyncManager) extractFightDataFromReportV2(reportData map[string]interface{}, fightID int, actorID int, startMs int64, endMs int64) (deaths, vulns, avoidable int, percentile float64, rawData []byte) {
	fightSpecificData := make(map[string]interface{})
	tables := make(map[string]interface{})
	// 顶层也放入基本元信息，方便下游直接读取
	fightSpecificData["fight_id"] = fightID
	fightSpecificData["start"] = startMs
	fightSpecificData["end"] = endMs
	fightSpecificData["meta"] = map[string]interface{}{
		"fight_id": fightID,
		"start":    startMs,
		"end":      endMs,
	}

	filterEntries := func(entries []interface{}) []interface{} {
		var out []interface{}
		for _, e := range entries {
			entry, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			if fid, ok := entry["fight"].(float64); ok && int(fid) == fightID {
				out = append(out, entry)
			}
		}
		return out
	}

	// 1. 死亡
	if dt, ok := reportData["deaths"].(map[string]interface{}); ok {
		if dData, ok := dt["data"].(map[string]interface{}); ok {
			if entries, ok := dData["entries"].([]interface{}); ok {
				fightDeaths := filterEntries(entries)
				for _, e := range fightDeaths {
					entry := e.(map[string]interface{})
					if idVal, ok := entry["id"].(float64); ok && int(idVal) == actorID {
						deaths++
					}
				}
				tables["deaths"] = fightDeaths
			}
		}
	}

	// 2. 易伤
	if vts, ok := reportData["debuffs"].(map[string]interface{}); ok {
		if vData, ok := vts["data"].(map[string]interface{}); ok {
			if entries, ok := vData["entries"].([]interface{}); ok {
				fightVulns := filterEntries(entries)
				for _, e := range fightVulns {
					entry := e.(map[string]interface{})
					if idVal, ok := entry["id"].(float64); ok && int(idVal) == actorID {
						if count, ok := entry["count"].(float64); ok {
							vulns += int(count)
						}
					}
				}
				tables["debuffs"] = fightVulns
			}
		}
	}

	// 3. Buffs
	if bt, ok := reportData["buffs"].(map[string]interface{}); ok {
		if bData, ok := bt["data"].(map[string]interface{}); ok {
			if entries, ok := bData["entries"].([]interface{}); ok {
				tables["buffs"] = filterEntries(entries)
			}
		}
	}

	// 4. Casts
	if ct, ok := reportData["casts"].(map[string]interface{}); ok {
		if cData, ok := ct["data"].(map[string]interface{}); ok {
			if entries, ok := cData["entries"].([]interface{}); ok {
				tables["casts"] = filterEntries(entries)
			}
		}
	}

	// 5. DamageDone
	if dt, ok := reportData["damageDone"].(map[string]interface{}); ok {
		if dData, ok := dt["data"].(map[string]interface{}); ok {
			if entries, ok := dData["entries"].([]interface{}); ok {
				tables["damageDone"] = filterEntries(entries)
			}
		}
	}

	// 6. Healing
	if ht, ok := reportData["healing"].(map[string]interface{}); ok {
		if hData, ok := ht["data"].(map[string]interface{}); ok {
			if entries, ok := hData["entries"].([]interface{}); ok {
				tables["healing"] = filterEntries(entries)
			}
		}
	}

	// 7. DamageTaken
	if dt, ok := reportData["damageTaken"].(map[string]interface{}); ok {
		if dData, ok := dt["data"].(map[string]interface{}); ok {
			if entries, ok := dData["entries"].([]interface{}); ok {
				tables["damageTaken"] = filterEntries(entries)
			}
		}
	}

	// 8. 可规避伤害 (avoidable)
	if at, ok := reportData["avoidable"].(map[string]interface{}); ok {
		if aData, ok := at["data"].(map[string]interface{}); ok {
			if entries, ok := aData["entries"].([]interface{}); ok {
				fightAvoidable := filterEntries(entries)
				for _, e := range fightAvoidable {
					entry := e.(map[string]interface{})
					if idVal, ok := entry["id"].(float64); ok && int(idVal) == actorID {
						if count, ok := entry["count"].(float64); ok {
							avoidable += int(count)
						}
					}
				}
				tables["avoidable"] = fightAvoidable
			}
		}
	}

	// 9. 百分位 (percentile)
	if rt, ok := reportData["rankings"].(map[string]interface{}); ok {
		if rData, ok := rt["data"].(map[string]interface{}); ok {
			if entries, ok := rData["entries"].([]interface{}); ok {
				for _, e := range entries {
					entry := e.(map[string]interface{})
					if fid, ok := entry["fight"].(float64); ok && int(fid) == fightID {
						if int(entry["id"].(float64)) == actorID {
							if pVal, ok := entry["percentile"].(float64); ok {
								percentile = pVal
								tables["percentile"] = pVal
								break
							}
						}
					}
				}
			}
		}
	}

	fightSpecificData["tables"] = tables
	rawData, _ = json.Marshal(fightSpecificData)
	return deaths, vulns, avoidable, percentile, rawData
}

// extractFightDataFromReport 为旧版，保留兼容但已不再使用
func (s *SyncManager) extractFightDataFromReport(reportData map[string]interface{}, fightID int, actorID int) (deaths, vulns, avoidable int, percentile float64, rawData []byte) {
	fightSpecificData := make(map[string]interface{})

	// 1. 提取死亡 (deaths)
	if dt, ok := reportData["deaths"].(map[string]interface{}); ok {
		if dData, ok := dt["data"].(map[string]interface{}); ok {
			if entries, ok := dData["entries"].([]interface{}); ok {
				var fightDeaths []interface{}
				for _, e := range entries {
					entry := e.(map[string]interface{})
					if fid, ok := entry["fight"].(float64); ok && int(fid) == fightID {
						fightDeaths = append(fightDeaths, entry)
						if int(entry["id"].(float64)) == actorID {
							deaths++
						}
					}
				}
				fightSpecificData["deaths"] = fightDeaths
			}
		}
	}

	// 2. 提取易伤 (debuffs)
	if vts, ok := reportData["debuffs"].(map[string]interface{}); ok {
		if vData, ok := vts["data"].(map[string]interface{}); ok {
			if entries, ok := vData["entries"].([]interface{}); ok {
				var fightVulns []interface{}
				for _, e := range entries {
					entry := e.(map[string]interface{})
					if fid, ok := entry["fight"].(float64); ok && int(fid) == fightID {
						fightVulns = append(fightVulns, entry)
						if int(entry["id"].(float64)) == actorID {
							if count, ok := entry["count"].(float64); ok {
								vulns += int(count)
							}
						}
					}
				}
				fightSpecificData["debuffs"] = fightVulns
			}
		}
	}

	// 3. 提取可规避伤害 (avoidable)
	if at, ok := reportData["avoidable"].(map[string]interface{}); ok {
		if aData, ok := at["data"].(map[string]interface{}); ok {
			if entries, ok := aData["entries"].([]interface{}); ok {
				var fightAvoidable []interface{}
				for _, e := range entries {
					entry := e.(map[string]interface{})
					if fid, ok := entry["fight"].(float64); ok && int(fid) == fightID {
						fightAvoidable = append(fightAvoidable, entry)
						if int(entry["id"].(float64)) == actorID {
							if count, ok := entry["count"].(float64); ok {
								avoidable += int(count)
							}
						}
					}
				}
				fightSpecificData["avoidable"] = fightAvoidable
			}
		}
	}

	// 4. 提取百分位 (percentile)
	if rt, ok := reportData["rankings"].(map[string]interface{}); ok {
		if rData, ok := rt["data"].(map[string]interface{}); ok {
			if entries, ok := rData["entries"].([]interface{}); ok {
				for _, e := range entries {
					entry := e.(map[string]interface{})
					if fid, ok := entry["fight"].(float64); ok && int(fid) == fightID {
						if int(entry["id"].(float64)) == actorID {
							if pVal, ok := entry["percentile"].(float64); ok {
								percentile = pVal
								fightSpecificData["percentile"] = pVal
								break
							}
						}
					}
				}
			}
		}
	}

	rawData, _ = json.Marshal(fightSpecificData)
	return deaths, vulns, avoidable, percentile, rawData
}

// 废弃旧的单战斗拉取函数
func (s *SyncManager) fetchFightTableData(reportCode string, fightID int, actorID int) (deaths, vulns, avoidable int, rawData []byte, err error) {
	// ... (代码已重构为批量模式)
	return 0, 0, 0, nil, nil
}
