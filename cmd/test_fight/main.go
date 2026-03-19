package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/config"
)

func main() {
	// 1. 解析参数
	reportCode := flag.String("code", "", "Qqm2wBR1gb6nkGy4")
	fightArg := flag.String("fight", "", "Fight ID (例如: 15) 或 'last' 获取最后一场")
	outFile := flag.String("out", "fight_data.json", "保存结果的 JSON 文件名")
	flag.Parse()

	if *reportCode == "" {
		fmt.Println("Usage: go run cmd/test_fight/main.go -code <Qqm2wBR1gb6nkGy4> -fight <last> [-out fight_data.json]")
		os.Exit(1)
	}

	// 2. 加载配置
	cfg := config.LoadConfig()
	client := api.NewFFLogsClient(cfg.FFLogsClientID, cfg.FFLogsClientSecret)

	// 3. 构建 GraphQL 查询
	// width optional fightIDs
	query := `query GetFightDetails($code: String!, $fightIDs: [Int]) {
		reportData {
			report(code: $code) {
				title
				startTime
				fights(fightIDs: $fightIDs) {
					id
					name
					difficulty
					gameZone {
						id
						name
					}
					startTime
					endTime
					kill
				}
				masterData {
					actors {
						id
						name
						gameID
						subType
					}
				}
			}
		}
	}`

	var fightIDs []int
	var targetFightID int = -1 // -1 means finding last, or unknown yet

	if *fightArg != "" && *fightArg != "last" {
		id, err := strconv.Atoi(*fightArg)
		if err != nil {
			log.Fatalf("无效的 fight 参数: %s", *fightArg)
		}
		fightIDs = []int{id}
		targetFightID = id
	}

	variables := map[string]interface{}{
		"code":     *reportCode,
		"fightIDs": nil, // 默认获取所有，后续在内存中筛选
	}
	if len(fightIDs) > 0 {
		variables["fightIDs"] = fightIDs
	}

	fmt.Printf("正在从 FFLogs 获取战斗数据 [Code: %s]...\n", *reportCode)

	// 4. 执行查询
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.ExecuteQuery(ctx, query, variables)
	if err != nil {
		log.Fatalf("API 请求失败: %v", err)
	}

	// 5. 解析结果
	reportData, ok := resp["reportData"].(map[string]interface{})
	if !ok {
		log.Fatalf("无法解析 reportData")
	}
	report, ok := reportData["report"].(map[string]interface{})
	if !ok {
		log.Fatalf("无法解析 report (可能 report code 无效)")
	}

	fights := report["fights"].([]interface{})
	masterData := report["masterData"].(map[string]interface{})
	actors := masterData["actors"].([]interface{})

	// 筛选目标 Fight
	var targetFight map[string]interface{}

	if len(fights) == 0 {
		log.Fatal("该 Report 没有包含任何战斗记录")
	}

	if *fightArg == "last" {
		// 获取最后一场
		targetFight = fights[len(fights)-1].(map[string]interface{})
	} else if len(fightIDs) > 0 {
		// 指定了 ID，通常 API 如果过滤生效，返回的 fights 应该只包含该 ID
		// 但如果 API 忽略了 invalid ID 可能会返回空。
		// 这里我们直接取第一个，或者遍历查找
		found := false
		for _, f := range fights {
			ft := f.(map[string]interface{})
			fid := int(ft["id"].(float64))
			if fid == targetFightID {
				targetFight = ft
				found = true
				break
			}
		}
		if !found && len(fights) == 1 {
			// 如果 API 确实只返回了一个，那应该就是它
			targetFight = fights[0].(map[string]interface{})
		} else if !found {
			log.Fatalf("未找到指定 FightID: %d", targetFightID)
		}
	} else {
		// 未指定 fight，列出列表供用户参考
		fmt.Println("未指定 -fight 参数，现有战斗列表如下:")
		for _, f := range fights {
			ft := f.(map[string]interface{})
			fid := int(ft["id"].(float64))
			name := ft["name"].(string)
			result := "Wipe"
			if kill, ok := ft["kill"].(bool); ok && kill {
				result = "Clear"
			}
			fmt.Printf(" - [ID: %d] %s (%s)\n", fid, name, result)
		}
		fmt.Println("\n请使用 -fight <ID> 或 -fight last 重试")
		return
	}

	fightName := targetFight["name"].(string)
	fmt.Printf("\n=== 战斗基本信息 ===\n")
	fmt.Printf("副本名称: %s\n", fightName)
	fmt.Printf("战斗ID: %d\n", int(targetFight["id"].(float64)))
	if diff, ok := targetFight["difficulty"].(float64); ok {
		fmt.Printf("难度: %.0f (100=Normal, 101=Savage)\n", diff)
	}
	if zone, ok := targetFight["gameZone"].(map[string]interface{}); ok {
		fmt.Printf("区域: %s (ID: %.0f)\n", zone["name"], zone["id"])
	}

	// 在 MasterData 中查找对应的 Actor GameID
	fmt.Printf("\n=== Boss Actor 信息 (用于 GameID 验证) ===\n")
	found := false
	for _, a := range actors {
		actor := a.(map[string]interface{})
		name := actor["name"].(string)

		// 简单的名字匹配，通常 Boss 名字和 Fight 名字一致或包含
		if name == fightName {
			found = true
			jsonBytes, _ := json.MarshalIndent(actor, "", "  ")
			fmt.Println(string(jsonBytes))
		}
	}

	if !found {
		fmt.Printf("未在 MasterData.Actors 中找到与战斗名称 [%s] 完全匹配的 NPC。\n", fightName)
		fmt.Println("尝试模糊匹配:")
		for _, a := range actors {
			actor := a.(map[string]interface{})
			name := actor["name"].(string)
			// 这里可以添加简单的包含检查
			// if strings.Contains(name, fightName) || strings.Contains(fightName, name) ...
			// 为了简洁，暂时只列出所有 NPC ID 信息供人工核对
			if gid, ok := actor["gameID"].(float64); ok {
				fmt.Printf("- Name: %-20s | GameID: %.0f | ID: %.0f\n", name, gid, actor["id"])
			}
		}
	}

	// 6. 保存数据到本地文件
	if *outFile != "" {
		output := map[string]interface{}{
			"reportCode":  *reportCode,
			"fight":       targetFight,
			"masterData":  masterData,
			"fetchedTime": time.Now().Format(time.RFC3339),
		}

		file, err := os.Create(*outFile)
		if err != nil {
			log.Fatalf("无法创建输出文件: %v", err)
		}
		defer file.Close()

		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(output); err != nil {
			log.Fatalf("写入文件失败: %v", err)
		}
		fmt.Printf("\n[成功] 战斗详情已保存至: %s\n", *outFile)
	}
}
