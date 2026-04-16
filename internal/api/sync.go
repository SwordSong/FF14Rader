package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cluster "github.com/user/ff14rader/internal/cluster"
	clusterserver "github.com/user/ff14rader/internal/cluster/server"
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

type scoreTask struct {
	mappingID  int
	reportCode string
	fightID    int
}

type clusterRegisterReportsPayload struct {
	Report string            `json:"report"`
	Entry  []reportHostEntry `json:"reports"`
	Reason string            `json:"reason,omitempty"`
}

const (
	clusterRegisterReasonV2Completed = "v2_parse_completed"
	clusterRegisterReasonDBPending   = "db_pending_unparsed_report"
)

type reportHostEntry struct {
	Events int    `json:"events"`
	Host   string `json:"host"`
}

// NewSyncManager 创建一个新的同步管理器
func NewSyncManager(client *FFLogsClient) *SyncManager {
	return &SyncManager{client: client, scorer: scoring.NewServiceFromEnv()}
}

// StartIncrementalSync 启动增量同步流程，包含 V2 查询、report code上报。
func (s *SyncManager) StartIncrementalSync(ctx context.Context, player models.PlayerLite, region string) error {
	var fullPlayer models.Player
	err := db.DB.Clauses(dbresolver.Write).Where("name = ? AND server = ? AND region = ?", player.Name, player.Server, region).First(&fullPlayer).Error
	if err != nil {
		return fmt.Errorf("failed to fetch full player data: %v", err)
	}
	//allowV2Query是一个布尔值，表示是否允许使用 V2 查询；reason是一个字符串，说明为什么允许或不允许使用 V2 查询。
	allowV2Query, reason, err := shouldRunV2ReportQuery(fullPlayer)
	if err != nil {
		return fmt.Errorf("check v2 query gate failed: %v", err)
	}
	if !allowV2Query {
		log.Printf("[INFO] 跳过 V2 reports 查询: %s", reason)
		reportHostAssignments, assignErr := s.buildPendingReportHostAssignments(fullPlayer.ID)
		if assignErr != nil {
			return fmt.Errorf("build reportcode-host assignments failed: %v", assignErr)
		}
		if err := postReportHostAssignmentsToClusterMaster(reportHostAssignments); err != nil {
			return fmt.Errorf("post reportcode-host assignments failed: %v", err)
		}
		return nil
	}
	log.Printf("[INFO] 执行 V2 reports 查询: %s", reason)

	return s.executeV2ReportsQuery(ctx, fullPlayer, region)
}

// executeV2ReportsQuery 执行 V2 报告分页查询、落库，并触发后续 V1 下载与能力刷新。
func (s *SyncManager) executeV2ReportsQuery(ctx context.Context, player models.Player, region string) error {
	serverSlug := player.Server
	// log.Printf("[INFO] DEBUG 查询: %+v", player)

	// 确保去重键的唯一索引存在（避免并发创建重复 master 行）
	// (name, timestamp) 足以区分同一场战斗
	_ = db.DB.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_fight_sync_maps_key ON fight_sync_maps (name, timestamp)")
	_ = db.DB.Exec("CREATE INDEX IF NOT EXISTS idx_fight_sync_maps_source_ids_gin ON fight_sync_maps USING GIN (source_ids)")

	// 启动时先自检：若 fights 已全下载但 reports 仍未标记，先修复一次
	if err := s.selfHealReportDownloadStatus(player.ID); err != nil {
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
		result := db.DB.Model(&models.FightSyncMap{}).Where("player_id = ?", player.ID).Order("timestamp desc").First(&lastSync)
		if result.Error == nil {
			startTime = lastSync.Timestamp * 1000
			log.Printf("发现历史记录，从时间戳 %d 开始增量拉取", startTime)
		} else {
			log.Println("未发现历史记录，将拉取全量数据")
		}
	}

	// 2. 构造 GraphQL 查询（分页 recentReports），向前拉取至截止日期（默认近 90 天）
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

	log.Printf("[DEBUG] Querying FFLogs: Name=%s, ServerSlug=%s, Region=%s, Page=%d, Limit=%d, Cutoff(ms)=%d", player.Name, serverSlug, region, 1, limit, cutoff)
	firstVars := map[string]interface{}{
		"name":   player.Name,
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
		if err := s.scorePendingDownloadedFights(ctx, player.ID); err != nil {
			log.Printf("[SCORE] pending score pass failed on empty report set: %v", err)
		}
		if err := markCompletedReportParseLogs(player.ID); err != nil {
			return err
		}
		if err := s.refreshPlayerOutputAbilityFromLogs(ctx, player, region); err != nil {
			return err
		}
		return touchPlayerV2ReportCheckedAt(player.ID)
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

					log.Printf("[DEBUG] Querying FFLogs: Name=%s, ServerSlug=%s, Region=%s, Page=%d, Limit=%d, Cutoff(ms)=%d", player.Name, serverSlug, region, page, limit, cutoff)
					vars := map[string]interface{}{
						"name":   player.Name,
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
	if err := s.syncAllReportsMetadata(player, allReports); err != nil {
		return err
	}
	// if _, err := s.downloadV1Reports(ctx, player.ID); err != nil {
	// 	return err
	// }
	// if err := s.refreshPlayerOutputAbilityFromLogs(ctx, player, region); err != nil {
	// 	return err
	// }
	// if err := touchPlayerV2ReportCheckedAt(player.ID); err != nil {
	// 	return err
	// }

	return nil
}

// selfHealReportDownloadStatus 自检并修复战斗与报告下载状态不一致问题。
func (s *SyncManager) selfHealReportDownloadStatus(playerID int) error {
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

// scorePendingDownloadedFights 对已下载但未完成评分的战斗执行补评分。
func (s *SyncManager) scorePendingDownloadedFights(ctx context.Context, playerID int) error {
	return s.scorePendingDownloadedFightsByReports(ctx, playerID, nil)
}

// scorePendingDownloadedFightsByReports 按可选报告范围筛选并并发执行待评分战斗。
func (s *SyncManager) scorePendingDownloadedFightsByReports(ctx context.Context, playerID int, reportCodes []string) error {
	if s.scorer == nil {
		return nil
	}

	var allowed map[string]struct{}
	if len(reportCodes) > 0 {
		allowed = make(map[string]struct{}, len(reportCodes))
		for _, code := range reportCodes {
			normalized := strings.ToUpper(strings.TrimSpace(code))
			if normalized == "" {
				continue
			}
			allowed[normalized] = struct{}{}
		}
		if len(allowed) == 0 {
			return nil
		}
	}

	var mappings []models.FightSyncMap
	if err := db.DB.Select("id", "master_id").
		Where("player_id = ? AND downloaded = ? AND (parsed_done = ? OR scored_at IS NULL OR scored_at < ?)", playerID, true, false, time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)).
		Find(&mappings).Error; err != nil {
		return fmt.Errorf("load pending scoring fights failed: %v", err)
	}
	if len(mappings) == 0 {
		return nil
	}

	tasks := make([]scoreTask, 0, len(mappings))
	for _, mapping := range mappings {
		code, fightID, ok := splitMasterID(mapping.MasterID)
		if !ok {
			log.Printf("[SCORE] skip invalid master_id: %s", mapping.MasterID)
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[strings.ToUpper(strings.TrimSpace(code))]; !ok {
				continue
			}
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
		go func(mappingID int, reportCode string, fightID int) {
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

// getScoreConcurrency 根据环境变量和评分服务建议值计算评分并发数。
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

// markFightParsedDoneIdempotent 幂等地将战斗标记为已解析完成。
func markFightParsedDoneIdempotent(mappingID int) (bool, error) {
	res := db.DB.Model(&models.FightSyncMap{}).
		Where("id = ? AND parsed_done = ?", mappingID, false).
		Update("parsed_done", true)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// markFightSkippedNoMatch 将无匹配角色的战斗标记为已处理并写入零分占位结果。
func markFightSkippedNoMatch(mappingID int) error {
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

// processSyncData 处理同步响应数据并提取报告列表。
func (s *SyncManager) processSyncData(playerID int, charName string, data map[string]interface{}) error {
	reports, _, err := extractReportsFromData(data)
	if err != nil {
		log.Printf("[DEBUG] GraphQL Response: %+v", data)
		return err
	}

	return s.processSyncReports(playerID, charName, reports)
}

// processSyncReports 处理报告列表并构建战斗映射写入数据库。
func (s *SyncManager) processSyncReports(playerID int, charName string, reports []interface{}) error {
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
				// if 返回if信息。
				// 	log.Printf("[WARN] 拉取事件失败，略过 events 缓存: %v", errEvents)
				// }

				// combinedRaw 返回combined原始数据信息。
				// if 返回if信息。
				// 	combinedRaw 返回combined原始数据信息。
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
	if err := postReportsToClusterMaster(uniqueCodes); err != nil {
		log.Printf("[CLUSTER] 上报 reportCode 失败 reports=%d err=%v", len(uniqueCodes), err)
	}

	log.Printf("多线程并行同步完成")
	return nil
}

// mergeEventPayload 将战斗详情数据和事件数据合并成一个新的 JSON 数据，方便后续存储和使用
func (s *SyncManager) writeReports(playerID int, masters []*masterFight, reportMetadata map[string]datatypes.JSON, reportSummaries map[string]reportSummary) error {
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
		// Ensure 返回ensure信息。
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

	// now 返回now信息。
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

// buildReportSummary 从单份报告数据提取标题与起止时间摘要。
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

// buildReportMetadata 构建并序列化需要持久化的报告元数据。
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

// getEncounterIDFilter 解析遭遇战 ID 过滤配置。
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

// extractReportsFromData 从响应数据中提取有效报告列表与分页信息。
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

// getPageConcurrency 读取分页拉取并发配置，缺省返回默认值。
func getPageConcurrency() int {
	const defaultConcurrency = 5
	if raw := os.Getenv("FFLOGS_SYNC_PAGE_CONCURRENCY"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return defaultConcurrency
}

// syncAllReportsMetadata 返回同步全量报告列表元数据。
func (s *SyncManager) syncAllReportsMetadata(player models.Player, reports []interface{}) error {
	if len(reports) == 0 {
		return nil
	}
	//这里的逻辑是先处理报告列表，提取报告代码，并与数据库中已有的报告解析日志进行对比，确定哪些报告是新的需要下载的 V1 报告，同时更新数据库中的报告解析日志状态，以便后续下载和处理。
	if err := s.processSyncReports(player.ID, player.Name, reports); err != nil {
		return err
	}

	// if 返回if信息。
	// 	return 返回return信息。
	// }
	// writePath 返回writepath信息。
	// if 返回if信息。
	// 	return 返回return信息。
	// }

	return nil
}

// finalizeAllReportsDownloads 收尾更新已下载报告状态与玩家报告集合。
func (s *SyncManager) finalizeAllReportsDownloads(playerID int, downloaded []string) error {
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

// mergeStringSets 合并两个字符串集合并去重。
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

// splitReportCode 拆分报告代码。
func splitReportCode(sourceID string) (string, bool) {
	idx := strings.LastIndex(sourceID, "-")
	if idx <= 0 {
		return "", false
	}
	return sourceID[:idx], true
}

// batchUpsertFightSyncMaps 批量写入或更新战斗映射并合并关键字段。
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

func clusterMasterEndpointFromEnv() string {
	raw := strings.TrimSpace(os.Getenv("CLUSTER_MASTER_ENDPOINT"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("CLUSTER_MASTER_URL"))
	}
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil || strings.TrimSpace(u.Host) == "" {
		return ""
	}
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

func normalizeReportCodesForClusterRegister(codes []string) []string {
	set := make(map[string]struct{}, len(codes))
	for _, raw := range codes {
		code := cluster.NormalizeReportCode(raw)
		if code == "" {
			continue
		}
		set[code] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for code := range set {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

func normalizeCandidateHosts(hosts []string) []string {
	seen := make(map[string]struct{}, len(hosts))
	out := make([]string, 0, len(hosts))
	for _, raw := range hosts {
		host := cluster.NormalizeHost(raw)
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}

func buildHostCapacities(candidateHosts []string, scorer *scoring.Service) map[string]int {
	capacities := make(map[string]int, len(candidateHosts))
	for _, host := range candidateHosts {
		capacities[host] = 1
	}

	if scorer == nil {
		return capacities
	}

	for rawHost, capacity := range scorer.CandidateAnalyzeHostCapacities() {
		host := cluster.NormalizeHost(rawHost)
		if host == "" {
			continue
		}
		if capacity <= 0 {
			capacity = 1
		}
		capacities[host] = capacity
	}

	return capacities
}

func buildHostLoadByEvents(reports map[string]reportHostEntry) map[string]int {
	load := make(map[string]int)
	for _, entry := range reports {
		host := cluster.NormalizeHost(entry.Host)
		if host == "" {
			continue
		}
		events := entry.Events
		if events <= 0 {
			events = 1
		}
		load[host] += events
	}
	return load
}

// chooseHostByRegistryRule 按 reportHost 语义选择主机：
// 1) host 为空（或未注册）时，调用方选择本机处理；
// 2) host 为本机且 events>0 时，继续本机处理，保持同 report 在单机解析；
// 3) 其余情况沿用已注册 host，避免跨机重复下载解析。
func chooseHostByRegistryRule(reportCode string, snapshot map[string]reportHostEntry, localHost string) string {
	local := cluster.NormalizeHost(localHost)
	if local == "" {
		local = "127.0.0.1"
	}

	code := cluster.NormalizeReportCode(reportCode)
	if code == "" {
		return local
	}

	entry, exists := snapshot[code]
	if !exists {
		entry, exists = snapshot[reportCode]
	}
	if !exists {
		return local
	}

	mapped := cluster.NormalizeHost(entry.Host)
	if mapped == "" {
		return local
	}
	if mapped == local && entry.Events > 0 {
		return local
	}

	return mapped
}

func chooseHostByCapacity(candidateHosts []string, hostLoad map[string]int, hostCapacities map[string]int, pendingEvents int) string {
	if len(candidateHosts) == 0 {
		return ""
	}
	if pendingEvents <= 0 {
		pendingEvents = 1
	}

	bestHost := ""
	bestScore := math.MaxFloat64
	bestProjected := int(^uint(0) >> 1)

	for _, host := range candidateHosts {
		capacity := hostCapacities[host]
		if capacity <= 0 {
			capacity = 1
		}

		projected := hostLoad[host] + pendingEvents
		score := float64(projected) / float64(capacity)
		if bestHost == "" ||
			score < bestScore ||
			(score == bestScore && (projected < bestProjected || (projected == bestProjected && host < bestHost))) {
			bestHost = host
			bestScore = score
			bestProjected = projected
		}
	}

	return bestHost
}

func fetchClusterReportsSnapshot(master string) (map[string]reportHostEntry, error) {
	_ = master

	snapshot := clusterserver.GlobalReportHostRegistry().Snapshot()
	if len(snapshot) == 0 {
		return map[string]reportHostEntry{}, nil
	}

	out := make(map[string]reportHostEntry, len(snapshot))
	for code, entry := range snapshot {
		out[code] = reportHostEntry{Events: entry.Events, Host: entry.Host}
	}
	return out, nil
}

// buildPendingReportHostAssignments 构建待处理报告的主机分配。
func (s *SyncManager) buildPendingReportHostAssignments(playerID int) (map[string]reportHostEntry, error) {
	pending, err := loadPendingFights(playerID)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return map[string]reportHostEntry{}, nil
	}

	grouped := groupPendingFights(pending)
	reportCodes := make([]string, 0, len(grouped))
	for code := range grouped {
		reportCodes = append(reportCodes, code)
	}
	sort.Strings(reportCodes)

	localHost := cluster.LocalHost()
	snapshot := make(map[string]reportHostEntry)
	if master := clusterMasterEndpointFromEnv(); master != "" {
		if fetched, err := fetchClusterReportsSnapshot(master); err != nil {
			log.Printf("[CLUSTER] 拉取报告分配快照失败 master=%s err=%v", master, err)
		} else {
			for code, entry := range fetched {
				snapshot[code] = entry
			}
		}
	}

	assignments := make(map[string]reportHostEntry, len(reportCodes))
	for _, code := range reportCodes {
		fightCount := len(grouped[code])
		assigned := chooseHostByRegistryRule(code, snapshot, localHost)
		assignments[code] = reportHostEntry{Events: fightCount, Host: assigned}
		normalizedCode := cluster.NormalizeReportCode(code)
		if normalizedCode != "" {
			snapshot[normalizedCode] = reportHostEntry{Events: fightCount, Host: assigned}
		}
		_, _ = clusterserver.GlobalReportHostRegistry().RegisterReportWithEvents(code, assigned, fightCount)
	}

	return assignments, nil
}

func postClusterReportsRegister(master string, payload clusterRegisterReportsPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	urlText := strings.TrimRight(master, "/") + "/api/cluster/reports/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlText, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	log.Printf("[CLUSTER] 上报报告注册结果 master=%s report=%s reason=%s entries=%d status=%d", master, payload.Report, strings.TrimSpace(payload.Reason), len(payload.Entry), resp.StatusCode)
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("register reports status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// postReportHostAssignmentsToClusterMaster 将报告主机分配结果上报给集群主节点，供集群内其他节点查询和使用。
func postReportHostAssignmentsToClusterMaster(assignments map[string]reportHostEntry) error {
	if len(assignments) == 0 {
		return nil
	}

	master := clusterMasterEndpointFromEnv()
	if master == "" {
		return nil
	}

	reportCodes := make([]string, 0, len(assignments))
	for code := range assignments {
		reportCodes = append(reportCodes, code)
	}
	sort.Strings(reportCodes)

	var firstErr error
	for _, rawCode := range reportCodes {
		code := cluster.NormalizeReportCode(rawCode)
		if code == "" {
			continue
		}
		entry := assignments[rawCode]
		payload := clusterRegisterReportsPayload{
			Report: code,
			// 注意：客户端通过 V2 解析得到的 reportHost 上报时，Host 必须留空。
			// 这里不要改成 entry.Host，避免客户端覆盖主节点的全局路由决策。
			Entry:  []reportHostEntry{{Events: entry.Events, Host: ""}},
			Reason: clusterRegisterReasonDBPending,
		}
		if err := postClusterReportsRegister(master, payload); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// postReportsToClusterMaster 将报告代码列表上报给集群主节点，供集群内其他节点查询和使用。
func postReportsToClusterMaster(codes []string) error {
	master := clusterMasterEndpointFromEnv()
	if master == "" {
		return nil
	}

	reports := normalizeReportCodesForClusterRegister(codes)
	if len(reports) == 0 {
		return nil
	}

	var firstErr error
	for _, code := range reports {
		payload := clusterRegisterReportsPayload{
			Report: code,
			// 同上：V2 解析结果上报仅传事件量，Host 始终留空。
			Entry:  []reportHostEntry{{Events: 0, Host: ""}},
			Reason: clusterRegisterReasonV2Completed,
		}
		if err := postClusterReportsRegister(master, payload); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
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

// resolveActorNameMap 构建角色 ID 到名称的映射。
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

// resolveFriendlyPlayerNames 解析并过滤友方玩家名称列表。
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

// shouldExcludeFriendlyPlayerName 判断名称是否应从友方玩家名单中过滤。
func shouldExcludeFriendlyPlayerName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == "multiple players" || n == "limit break" || n == "limitbreak"
}

// isFriendPlayersUsable 判断友方玩家名单是否满足开荒速度计算条件。
func isFriendPlayersUsable(names []string) bool {
	// 8人本要求完整队伍名单（含自己）才用于开荒速度计算。
	return len(names) == 8
}

// parseFriendlyPlayerIDs 解析并去重友方玩家 ID 列表。
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

// parseFriendlyPlayerID 解析单个友方玩家项为玩家 ID。
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

// containsString 判断字符串是否满足条件。
// containsString 检查字符串切片是否包含目标值。
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

// resolveActorInfo 从报告数据中解析玩家职业与角色 ID。
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

// mapFloor 将战斗名称映射为楼层标识并判断是否为目标战斗。
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

// buildMasterFights 构建主节点战斗列表。
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

// groupMastersByReport 按报告编号对主战斗集合分组。
func groupMastersByReport(masters []*masterFight) map[string][]*masterFight {
	grouped := make(map[string][]*masterFight)
	for _, master := range masters {
		grouped[master.reportCode] = append(grouped[master.reportCode], master)
	}
	return grouped
}

// findMatchingMaster 在现有主战斗集合中查找可合并战斗。
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

// absInt64 返回 64 位整数的绝对值。
func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// EnsurePlayerID 确保玩家记录存在，返回玩家 ID；如果不存在则创建新记录
func EnsurePlayerID(name, server, region string) (models.PlayerLite, string, error) {
	name = strings.TrimSpace(name)
	server = strings.TrimSpace(server)
	region = strings.TrimSpace(region)
	if region == "" {
		region = "CN"
	}

	var player models.Player
	query := db.DB.Clauses(dbresolver.Write).
		Select("id", "pichash").
		Where("name = ? AND server = ? AND region = ?", name, server, region).
		Limit(1).
		Find(&player)
	if query.Error != nil {
		return models.PlayerLite{}, "", query.Error
	}

	playerLite := models.PlayerLite{
		PlayerID: player.ID,
		Name:     name,
		Server:   server,
		Region:   region,
	}
	if query.RowsAffected > 0 {
		playerLite.NewPlayer = false
		return playerLite, player.PicHash, nil
	}

	newPlayer := models.Player{
		Name:   name,
		Server: server,
		Region: region,
	}
	if errCreate := db.DB.Clauses(dbresolver.Write).Select("name", "server", "region").Create(&newPlayer).Error; errCreate != nil {
		return models.PlayerLite{}, "", errCreate
	}

	return models.PlayerLite{
		PlayerID:  newPlayer.ID,
		NewPlayer: true,
		Name:      name,
		Server:    server,
		Region:    region,
	}, "", nil
}

// shouldRunV2ReportQuery 根据玩家状态和更新时间判断是否执行 V2 报告查询。
func shouldRunV2ReportQuery(player models.Player) (bool, string, error) {
	if player.ID == 0 {
		return true, "player_id=0 fallback", nil
	}
	if player.NewPlayer {
		return true, "新创建玩家，首次同步直接查询", nil
	}

	var playerRecord models.Player
	if err := db.DB.Select("id", "created_at", "updated_at").Where("id = ?", player.ID).First(&playerRecord).Error; err != nil {
		return false, "", err
	}

	if playerRecord.UpdatedAt.IsZero() {
		return true, "updated_at 为空，执行查询", nil
	}

	if playerRecord.CreatedAt.Equal(playerRecord.UpdatedAt) {
		if time.Since(playerRecord.CreatedAt) < 24*time.Hour {
			return true, "新创建玩家（created_at==updated_at 且 <24h）", nil
		}
	}

	age := time.Since(playerRecord.UpdatedAt)
	if age >= 24*time.Hour {
		return true, fmt.Sprintf("updated_at 超过24小时（%.1fh）", age.Hours()), nil
	}

	return false, fmt.Sprintf("updated_at 距今 %.1fh，未超过24小时", age.Hours()), nil
}

// touchPlayerV2ReportCheckedAt 更新玩家 V2 查询检查时间。
func touchPlayerV2ReportCheckedAt(playerID int) error {
	if playerID == 0 {
		return nil
	}
	return db.DB.Model(&models.Player{}).
		Where("id = ?", playerID).
		UpdateColumn("updated_at", time.Now()).Error
}

// refreshPlayerOutputAbilityFromLogs 拉取并更新玩家输出能力评分。
func (s *SyncManager) refreshPlayerOutputAbilityFromLogs(ctx context.Context, player models.Player, region string) error {
	name := strings.TrimSpace(player.Name)
	server := strings.TrimSpace(player.Server)
	region = strings.TrimSpace(region)
	if player.ID == 0 || name == "" || server == "" || region == "" {
		return nil
	}

	ability, err := s.fetchCharacterOutputAbility(ctx, name, server, region)
	if err != nil {
		return err
	}

	return db.DB.Model(&models.Player{}).
		Where("id = ?", player.ID).
		UpdateColumn("output_ability", ability).Error
}

// fetchCharacterOutputAbility 查询并提取角色输出能力评分。
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

// extractOutputAbilityFromZoneRankings 从分区排行结构中提取输出能力值。
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

// extractPercentileFromRankingEntries 从排名条目集合计算平均百分位。
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

// clampPercent 将百分值限制在 0-100 并保留两位小数。
func clampPercent(v float64) float64 {
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return math.Round(v*100) / 100
}

// parseRankingEntryFloat 按候选键从排名条目中解析浮点值。
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
