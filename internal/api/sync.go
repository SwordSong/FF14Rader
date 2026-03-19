package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
	"gorm.io/plugin/dbresolver"
)

// SyncManager 同步管理器
type SyncManager struct {
	client *FFLogsClient
}

func NewSyncManager(client *FFLogsClient) *SyncManager {
	return &SyncManager{client: client}
}

// StartIncrementalSync 开始增量同步
func (s *SyncManager) StartIncrementalSync(ctx context.Context, playerID uint, name, server, region string) error {
	log.Printf("开始同步玩家 [%s-%s] 的数据...", name, server)

	// 确保去重键的唯一索引存在（避免并发创建重复 master 行）
	// (boss_name, timestamp, duration) 足以区分同一场战斗
	_ = db.DB.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_fight_sync_maps_key ON fight_sync_maps (boss_name, timestamp, duration)")

	// 1. 获取数据库中该玩家最后一条记录的时间
	var lastReport models.Report
	var startTime int64 = 0
	
	// 允许通过环境变量或配置设定一个固定的起步时间 (Unix 毫秒)
	// 例如：1722441600000 (2024-08-01)
	const manualStartTime int64 = 1767686400000 // 修正为毫秒，对应你之前设置的 2026 年初

	if manualStartTime > 0 {
		startTime = manualStartTime
		log.Printf("使用硬编码起始时间 (毫秒): %d", startTime)
	} else {
		result := db.DB.Where("player_id = ?", playerID).Order("start_time desc").First(&lastReport)
		if result.Error == nil {
			startTime = lastReport.StartTime
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
				lodestoneID
				recentReports(limit: $limit, page: $page) {
					data {
						code
						title
						startTime
						masterData {
							actors {
								id
								name
								subType
								gameID
							}
						}
						fights {
							id
							name
							gameZone {
								id
								name
							}
							kill
							endTime
							startTime
							difficulty
							bossPercentage
							friendlyPlayers
						}
					}
				}
			}
		}
	}`

	for page := 1; ; page++ {
		log.Printf("[DEBUG] Querying FFLogs: Name=%s, ServerSlug=%s, Region=%s, Page=%d, Limit=%d, Cutoff(ms)=%d", name, serverSlug, region, page, limit, cutoff)

		variables := map[string]interface{}{
			"name":   name,
			"server": serverSlug,
			"region": region,
			"limit":  limit,
			"page":   page,
		}

		data, err := s.client.ExecuteQuery(ctx, query, variables)
		if err != nil {
			return fmt.Errorf("同步失败: %v", err)
		}

		// 3. 解析并存入数据库
		if err := s.processSyncData(playerID, name, data); err != nil {
			return err
		}

		// 4. 检查是否达到截止日期或没有更多数据
		oldest, count := s.getOldestReportTime(data)
		if count == 0 {
			break
		}
		if oldest <= cutoff {
			log.Printf("[INFO] 已达到截止日期，最早报告时间: %d (ms)", oldest)
			break
		}
	}

	return nil
}

func (s *SyncManager) processSyncData(playerID uint, charName string, data map[string]interface{}) error {
	charData, ok := data["characterData"].(map[string]interface{})
	if !ok || charData["character"] == nil {
		log.Printf("[DEBUG] GraphQL Response: %+v", data)
		return fmt.Errorf("未找到玩家数据 (charName: %s)", charName)
	}

	character := charData["character"].(map[string]interface{})
	
	// 兼容 reports 和 recentReports 字段
	var reportsData map[string]interface{}
	var okReports bool
	if reportsData, okReports = character["reports"].(map[string]interface{}); !okReports {
		if reportsData, okReports = character["recentReports"].(map[string]interface{}); !okReports {
			return fmt.Errorf("未找到报告列表")
		}
	}
	
	if reportsData["data"] == nil {
		return fmt.Errorf("未找到报告数据内容")
	}

	reports := reportsData["data"].([]interface{})

	// --- 批量拉取战斗详情 (多线程加速) ---
	var wg sync.WaitGroup
	// 使用信号量控制并发请求数，避免被 FFLogs 封禁或触发过度限制
	sem := make(chan struct{}, 8)

	for _, r := range reports {
		reportMap := r.(map[string]interface{})
		reportCode := reportMap["code"].(string)
		reportTitle := reportMap["title"].(string)
		reportStart := int64(reportMap["startTime"].(float64))
		fights := reportMap["fights"].([]interface{})

		wg.Add(1)
		go func(code, title string, start int64, fs []interface{}, rMap map[string]interface{}) {
			defer wg.Done()
			sem <- struct{}{}        // 获取信号量
			defer func() { <-sem }() // 释放信号量

			// 1. 解析职业信息 (从 masterData.actors 中找到该玩家)
			var playerJob string
			var actorID int
			var actorGameIDs = make(map[string]int) // 存储所有 Actor 的 GameID (Name -> GameID)

			if md, ok := rMap["masterData"].(map[string]interface{}); ok {
				if actors, ok := md["actors"].([]interface{}); ok {
					for _, a := range actors {
						actor := a.(map[string]interface{})
						aName := actor["name"].(string)

						// 收集 GameID
						if gid, ok := actor["gameID"].(float64); ok {
							actorGameIDs[aName] = int(gid)
							// 对于 Boss 可能会有重名，但这通常是按 ID 区分的，我们这里简单映射 Name -> GameID
							// 注意：同一个名字可能有多个 Actor (如小怪或分身)，GameID 可能不同
							// 但 Boss 通常具有唯一的 GameID，或者我们关心的 Boss ID 是特定的。
						}

						if aName == charName {
							playerJob = actor["subType"].(string)
							actorID = int(actor["id"].(float64))
						}
					}
				}
			}

			if actorID == 0 {
				return
			}

			// 2. 检查是否有本人参与的战斗
			hasMe := false
			for _, f := range fs {
				fight := f.(map[string]interface{})
				if s.isActorInFight(fight, actorID) {
					hasMe = true
					break
				}
			}
			if !hasMe {
				return
			}

			// 3. 批量拉取整份 Report 的 Table 数据 (优化网络请求)
			reportTableData, err := s.fetchFullReportTableData(code)
			if err != nil {
				log.Printf("拉取报告 %s 详情失败: %v", code, err)
				return
			}

			// 4. 处理每一场战斗
			for _, f := range fs {
				fight := f.(map[string]interface{})
				fightID := int(fight["id"].(float64))

				// 只保存零式（Difficulty 101/100）且本人参战的记录
				difficultyVal := fight["difficulty"]
				if difficultyVal == nil {
					continue
				}
				diff := int(difficultyVal.(float64))

				// 简化逻辑：只处理难度为 101 (Savage) 的战斗
				// 100 = Normal, 101 = Savage
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
				fightTimestamp := start/1000 + int64(fStart/1000)
				reportFightID := fmt.Sprintf("%s-%d", code, fightID)

				// --- 核心过滤逻辑：基于名称映射 ---
				bossName := fight["name"].(string) 
				
				// 映射表：将 boss 名称映射为规范的代号 (M9S, M10S, M11S, M12S)
				// 既然已经前置判断了 Difficulty=101，这里的名称只要匹配上，就是零式
				bossMapping := map[string]string{
					"Vamp Fatale":              "M9S",
					"Red Hot / Deep Blue":      "M10S",
					"The Tyrant":               "M11S",
					"Lindwurm":                 "M12S",
				}
				
				mappedName, isTarget := bossMapping[bossName]
				if !isTarget {
					continue
				}
				bossName = mappedName

				// 打印日志以便调试
				log.Printf("[SYNC] 处理战斗: 战斗数据集：%s 战斗数据：第%s个  (原名: %s, 难度: %d, 时间: %s)", 
					reportCode,reportFightID, fight["name"], diff, 
					time.Unix(fightTimestamp, 0).Format("2006-01-02 15:04:05"))

				// 4. GameZoneID 辅助日志 (仅调试)
				var zoneID int
				// 获取 ZoneID 并过滤（7.0 阿卡迪亚通常具有相同的 ZoneID）
				// 注意：这里需要观察日志来确定 ZoneID，暂时只打 Log
				if zoneMap, ok := fight["gameZone"].(map[string]interface{}); ok {
					if zid, ok := zoneMap["id"].(float64); ok {
						zoneID = int(zid)
					}
				}
				_ = zoneID


				// --- 详情缓存检查与去重映射 (FightSyncMap) ---
				// 1. 检查是否存在去重映射关系 (查重逻辑)
				var syncMap models.FightSyncMap
				var targetMasterID string

				// 在 5s 容差范围内查找相同副本的战斗记录
				// 关键点：强制在写库查找，避免多线程并发下的读写分离导致主从解析延迟
				var errSearch error
				errSearch = db.DB.Clauses(dbresolver.Write).Where("boss_name = ? AND ABS(timestamp - ?) <= 5 AND ABS(duration - ?) <= 5",
					bossName, fightTimestamp, duration).First(&syncMap).Error

				if errSearch == nil {
					// 命中重复，检查当前 ID 是否已在列表中，不在则加入
					targetMasterID = syncMap.MasterID
					if !containsString(syncMap.SourceIDs, reportFightID) {
						syncMap.SourceIDs = append(syncMap.SourceIDs, reportFightID)
						if errUpdate := db.DB.Model(&syncMap).Update("source_ids", syncMap.SourceIDs).Error; errUpdate != nil {
							log.Printf("[ERROR] 更新映射表失败: %v", errUpdate)
						}
						log.Printf("[SYNC] 发现重复战斗记录，已加入映射列表: %s -> %s (总数: %d)", reportFightID, targetMasterID, len(syncMap.SourceIDs))
					}
				} else {
					// 未命中的新战斗，尝试创建新的映射记录，将自己作为 Master
					targetMasterID = reportFightID
					newSyncMap := models.FightSyncMap{
						MasterID:  reportFightID,
						SourceIDs: pq.StringArray{reportFightID},
						BossName:  bossName,
						Timestamp: fightTimestamp,
						Duration:  duration,
					}
					if errCreate := db.DB.Table("fight_sync_maps").Create(&newSyncMap).Error; errCreate != nil {
						// 并发竞争：唯一索引冲突，回查已存在记录并合并 source_ids
						if strings.Contains(errCreate.Error(), "duplicate key") {
							if errRetry := db.DB.Clauses(dbresolver.Write).Where("boss_name = ? AND ABS(timestamp - ?) <= 5 AND ABS(duration - ?) <= 5",
								bossName, fightTimestamp, duration).First(&syncMap).Error; errRetry == nil {
								targetMasterID = syncMap.MasterID
								if !containsString(syncMap.SourceIDs, reportFightID) {
									syncMap.SourceIDs = append(syncMap.SourceIDs, reportFightID)
									if errUpdate := db.DB.Model(&syncMap).Update("source_ids", syncMap.SourceIDs).Error; errUpdate != nil {
										log.Printf("[ERROR] 并发合并 source_ids 失败: %v", errUpdate)
									}
									log.Printf("[SYNC] 并发冲突后合并: %s -> %s (总数: %d)", reportFightID, targetMasterID, len(syncMap.SourceIDs))
								}
							} else {
								log.Printf("[ERROR] 创建映射记录失败: %v", errCreate)
							}
						} else {
							log.Printf("[SYNC] 已创建新战斗映射: %s (Boss: %s, Time: %d)", reportFightID, bossName, fightTimestamp)
						}
					}
				}

				// 2. 只有当自己就是 MasterID 时，才抓取并保存全量 Data 详情
				var deaths, vulns, avoidable int
				var percentile float64
				var cache models.FightCache

				if targetMasterID == reportFightID {
					// 检查详情缓存
					if db.DB.Where("id = ?", reportFightID).First(&cache).Error == nil {
						deaths = cache.Deaths
						vulns = cache.VulnStacks
						avoidable = cache.AvoidableDamage
						percentile = cache.Percentile
					} else {
						d, v, a, p, rawData := s.extractFightDataFromReport(reportTableData, fightID, actorID)
						deaths, vulns, avoidable, percentile = d, v, a, p

						db.DB.Create(&models.FightCache{
							ID:              reportFightID,
							Duration:        int(fEnd - fStart),
							Deaths:          deaths,
							VulnStacks:      vulns,
							AvoidableDamage: avoidable,
							Percentile:      p,
							Timestamp:       fightTimestamp,
							Data:            rawData,
						})
					}
				} else {
					// 如果不是 Master，我们依然提取当前玩家的个人表现（用于 Reports 表），但不存 Data Blob
					deaths, vulns, avoidable, percentile, _ = s.extractFightDataFromReport(reportTableData, fightID, actorID)
				}

				// 3. 只有当自己就是 MasterID 时，才作为权威记录存入 reports 表
				// 如果是重复记录，我们已经在 fight_sync_maps 中建立了关联，无需在 reports 表中冗余
				if reportFightID == targetMasterID {
					bossPercentage := 0.0
					if bp, ok := fight["bossPercentage"].(float64); ok {
						bossPercentage = bp
					}

					newReport := models.Report{
						ID:              reportFightID,
						PlayerID:        playerID,
						Title:           title,
						StartTime:       fightTimestamp,
						Duration:        duration,
						FightID:         fightID,
						Kill:            fight["kill"].(bool),
						BossName:        bossName,
						Job:             playerJob,
						WipeProgress:    bossPercentage,
						Deaths:          deaths,
						VulnStacks:      vulns,
						AvoidableDamage: avoidable,
						Percentile:      percentile,
					}

					if err := db.DB.Save(&newReport).Error; err != nil {
						log.Printf("保存报告 %s 失败: %v", newReport.ID, err)
					}
				} else {
					log.Printf("[SYNC] 跳过重复记录入库 (Reports表): %s -> %s", reportFightID, targetMasterID)
				}
			}
		}(reportCode, reportTitle, reportStart, fights, reportMap)
	}

	wg.Wait()
	log.Printf("多线程并行同步完成")
	return nil
}

// 获取当前批次中最早的报告时间（毫秒），用于分页终止条件
func (s *SyncManager) getOldestReportTime(data map[string]interface{}) (oldestMs int64, count int) {
	oldestMs = 1<<63 - 1

	charData, ok := data["characterData"].(map[string]interface{})
	if !ok || charData["character"] == nil {
		return 0, 0
	}
	character := charData["character"].(map[string]interface{})
	reportsData, ok := character["recentReports"].(map[string]interface{})
	if !ok || reportsData["data"] == nil {
		return 0, 0
	}
	reports := reportsData["data"].([]interface{})
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

// 辅助函数：检查 Actor 是否在某场战斗中
func (s *SyncManager) isActorInFight(fight map[string]interface{}, actorID int) bool {
	if friends, ok := fight["friendlyPlayers"].([]interface{}); ok {
		for _, fid := range friends {
			if int(fid.(float64)) == actorID {
				return true
			}
		}
	}
	return false
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
