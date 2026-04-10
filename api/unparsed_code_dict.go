package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/user/ff14rader/internal/cluster"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
	"gorm.io/gorm"
)

type unparsedCodeRow struct {
	PlayerID uint   `gorm:"column:player_id"`
	MasterID string `gorm:"column:master_id"`
}

type upsertUnparsedCodesRequest struct {
	Host     string           `json:"host,omitempty"`
	PlayerID uint             `json:"playerId"`
	Username string           `json:"username,omitempty"`
	Server   string           `json:"server,omitempty"`
	Codes    map[string][]int `json:"codes"`
}

func splitMasterReportAndFightID(masterID string) (string, int, bool) {
	text := strings.TrimSpace(masterID)
	idx := strings.LastIndex(text, "-")
	if idx <= 0 || idx >= len(text)-1 {
		return "", 0, false
	}
	code := cluster.NormalizeReportCode(text[:idx])
	if code == "" {
		return "", 0, false
	}
	fightID, err := strconv.Atoi(text[idx+1:])
	if err != nil || fightID <= 0 {
		return "", 0, false
	}
	return code, fightID, true
}

func dedupeSortFightIDs(ids []int) []int {
	if len(ids) == 0 {
		return nil
	}
	set := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		set[id] = struct{}{}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]int, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func normalizeCodeFightMap(input map[string][]int) map[string][]int {
	if len(input) == 0 {
		return map[string][]int{}
	}
	out := make(map[string][]int, len(input))
	for rawCode, fightIDs := range input {
		code := cluster.NormalizeReportCode(rawCode)
		if code == "" {
			continue
		}
		normalized := dedupeSortFightIDs(fightIDs)
		if existing, ok := out[code]; ok {
			out[code] = dedupeSortFightIDs(append(existing, normalized...))
			continue
		}
		out[code] = normalized
	}
	return out
}

func collectUnparsedCodeDictFromRows(rows []unparsedCodeRow) map[uint]map[string][]int {
	out := make(map[uint]map[string][]int)
	for _, row := range rows {
		if row.PlayerID == 0 {
			continue
		}
		code, fightID, ok := splitMasterReportAndFightID(row.MasterID)
		if !ok {
			continue
		}
		if _, exists := out[row.PlayerID]; !exists {
			out[row.PlayerID] = make(map[string][]int)
		}
		out[row.PlayerID][code] = append(out[row.PlayerID][code], fightID)
	}

	for playerID, codeMap := range out {
		for code, fightIDs := range codeMap {
			out[playerID][code] = dedupeSortFightIDs(fightIDs)
		}
	}
	return out
}

func buildUnparsedCodeDictFromDB() (map[uint]map[string][]int, error) {
	var rows []unparsedCodeRow
	if err := db.DB.Model(&models.FightSyncMap{}).
		Select("player_id", "master_id").
		Where("parsed_done = ?", false).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return collectUnparsedCodeDictFromRows(rows), nil
}

func totalCodesInDict(dict map[uint]map[string][]int) int {
	total := 0
	for _, codeMap := range dict {
		total += len(codeMap)
	}
	return total
}

// RebuildUnparsedCodeDictFromDB 启动时从 fight_sync_maps(parsed_done=false) 重建“用户未解析code字典”。
func RebuildUnparsedCodeDictFromDB() (int, int, error) {
	dict, err := buildUnparsedCodeDictFromDB()
	if err != nil {
		return 0, 0, err
	}
	cluster.GlobalUnparsedCodeDict().ReplaceAll(dict)
	return len(dict), totalCodesInDict(dict), nil
}

func collectPlayerUnparsedCodesByPlayerID(playerID uint) (map[string][]int, error) {
	if playerID == 0 {
		return map[string][]int{}, nil
	}
	var rows []unparsedCodeRow
	if err := db.DB.Model(&models.FightSyncMap{}).
		Select("player_id", "master_id").
		Where("player_id = ? AND parsed_done = ?", playerID, false).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	all := collectUnparsedCodeDictFromRows(rows)
	if byPlayer, ok := all[playerID]; ok {
		return byPlayer, nil
	}
	return map[string][]int{}, nil
}

func collectPlayerUnparsedCodesByNameServer(username, server string) (uint, map[string][]int, error) {
	name := strings.TrimSpace(username)
	world := strings.TrimSpace(server)
	if name == "" || world == "" {
		return 0, map[string][]int{}, nil
	}

	var player models.Player
	err := db.DB.Model(&models.Player{}).
		Select("id", "updated_at").
		Where("LOWER(name) = LOWER(?) AND LOWER(server) = LOWER(?)", name, world).
		Order("updated_at DESC").
		First(&player).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, map[string][]int{}, nil
		}
		return 0, nil, err
	}

	codeMap, mapErr := collectPlayerUnparsedCodesByPlayerID(player.ID)
	if mapErr != nil {
		return 0, nil, mapErr
	}
	return player.ID, codeMap, nil
}

func applyUnparsedCodesToServer(playerID uint, preferredHost string, codeMap map[string][]int) (int, int, int, int) {
	if playerID == 0 {
		return 0, 0, 0, 0
	}
	normalizedMap := normalizeCodeFightMap(codeMap)
	upsertedCodes, _ := cluster.GlobalUnparsedCodeDict().UpsertPlayer(playerID, normalizedMap)
	if len(normalizedMap) == 0 {
		return upsertedCodes, 0, 0, 0
	}

	reportCodes := make([]string, 0, len(normalizedMap))
	for code := range normalizedMap {
		reportCodes = append(reportCodes, code)
	}
	sort.Strings(reportCodes)

	hostToCodes := make(map[string][]string)
	candidateHosts := collectOnlineWarmupCandidateHosts(time.Now())
	missingHost := 0
	for _, code := range reportCodes {
		host := cluster.NormalizeHost(preferredHost)
		if host == "" {
			if resolvedHost, ok := cluster.GlobalReportHostRegistry().ResolveHost(code); ok {
				host = cluster.NormalizeHost(resolvedHost)
			}
		}
		if host == "" {
			host = cluster.GlobalReportHostRegistry().AssignHostForReport(code, 1, candidateHosts)
		}
		if host == "" {
			missingHost++
			continue
		}
		cluster.GlobalReportHostRegistry().RestoreReportHost(code, host)
		hostToCodes[host] = append(hostToCodes[host], code)
	}

	queued := 0
	alreadyQueued := 0
	for host, codes := range hostToCodes {
		normalizedHost := cluster.NormalizeHost(host)
		if normalizedHost == "" || len(codes) == 0 {
			continue
		}
		sort.Strings(codes)
		if _, err := cluster.PersistReportHostMappings(normalizedHost, codes, time.Now()); err != nil {
			log.Printf("[UNPARSED] 持久化 report->host 失败 host=%s reports=%d err=%v", normalizedHost, len(codes), err)
		}
		for _, code := range codes {
			added, enqueueErr := cluster.GlobalDispatchTaskQueue().EnqueueReports(playerID, normalizedHost, []string{code})
			if enqueueErr != nil {
				log.Printf("[UNPARSED] 入队失败 player=%d code=%s host=%s err=%v", playerID, code, normalizedHost, enqueueErr)
				continue
			}
			if added > 0 {
				queued += added
			} else {
				alreadyQueued++
			}
		}
	}

	return upsertedCodes, queued, alreadyQueued, missingHost
}

func submitUnparsedCodesToMaster(playerID uint, username, server string, codeMap map[string][]int) error {
	if playerID == 0 {
		return nil
	}
	normalizedMap := normalizeCodeFightMap(codeMap)
	if len(normalizedMap) == 0 {
		return nil
	}

	master := strings.TrimSpace(cluster.ClusterMasterEndpoint())
	if master == "" {
		return nil
	}

	payload := upsertUnparsedCodesRequest{
		Host:     cluster.LocalHost(),
		PlayerID: playerID,
		Username: strings.TrimSpace(username),
		Server:   strings.TrimSpace(server),
		Codes:    normalizedMap,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	urlText := strings.TrimRight(master, "/") + "/api/cluster/unparsed-codes/upsert"
	req, err := http.NewRequest(http.MethodPost, urlText, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		return fmt.Errorf("submit unparsed codes status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (s *Service) upsertUnparsedCodesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed. Only POST is supported.", http.StatusMethodNotAllowed)
		return
	}

	var req upsertUnparsedCodesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %v", err))
		return
	}
	if req.PlayerID == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("playerId is required"))
		return
	}

	host := cluster.NormalizeHost(req.Host)
	if host == "" {
		host = cluster.NormalizeHost(r.RemoteAddr)
	}

	upsertedCodes, queued, alreadyQueued, missingHost := applyUnparsedCodesToServer(req.PlayerID, host, req.Codes)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "ok",
		"playerId":      req.PlayerID,
		"host":          host,
		"upsertedCodes": upsertedCodes,
		"queued":        queued,
		"alreadyQueued": alreadyQueued,
		"missingHost":   missingHost,
		"time":          time.Now().Format(time.RFC3339),
	})
}

func (s *Service) listUnparsedCodesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Only GET is supported.", http.StatusMethodNotAllowed)
		return
	}

	players, codes, fights, updatedAt := cluster.GlobalUnparsedCodeDict().Stats()
	payload := map[string]interface{}{
		"status":     "ok",
		"players":    players,
		"codes":      codes,
		"fights":     fights,
		"updatedAt":  "",
		"dictionary": cluster.GlobalUnparsedCodeDict().Snapshot(),
		"queueSize":  len(cluster.GlobalDispatchTaskQueue().Snapshot()),
		"serverTime": time.Now().Format(time.RFC3339),
	}
	if !updatedAt.IsZero() {
		payload["updatedAt"] = updatedAt.Format(time.RFC3339)
	}

	writeJSON(w, http.StatusOK, payload)
}
