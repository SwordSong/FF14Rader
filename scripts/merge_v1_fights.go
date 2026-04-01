package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type fightsResponse struct {
	Code   string      `json:"code"`
	Start  int64       `json:"start"`
	End    int64       `json:"end"`
	Title  string      `json:"title"`
	Fights []fightInfo `json:"fights"`
}

type fightInfo struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	EncounterID int    `json:"encounterID"`
	StartTime   int64  `json:"start_time"`
	EndTime     int64  `json:"end_time"`
}

type sourceFight struct {
	ReportCode  string `json:"report_code"`
	FightID     int    `json:"fight_id"`
	AbsStart    int64  `json:"abs_start"`
	DurationMS  int64  `json:"duration_ms"`
	EncounterID int    `json:"encounter_id"`
	FightName   string `json:"fight_name"`
}

type masterFight struct {
	MasterID    string        `json:"master_id"`
	EncounterID int           `json:"encounter_id"`
	Name        string        `json:"name"`
	Start       int64         `json:"start"`
	DurationMS  int64         `json:"duration_ms"`
	Sources     []sourceFight `json:"sources"`
}

type mappingOutput struct {
	ReportMappings map[string]map[string]string `json:"report_mappings"`
	Masters        []masterFight                `json:"masters"`
}

type sourceEvents struct {
	Source  sourceFight
	Events  []map[string]interface{}
	HasPref bool
}

func main() {
	rootDir := flag.String("root", "./downloads/fflogs", "root directory containing report folders")
	outDir := flag.String("out", "./downloads/fflogs/merged", "output directory")
	toleranceMS := flag.Int64("tolerance-ms", 5000, "tolerance window in milliseconds")
	preferIDs := flag.String("prefer-player-id", "", "comma-separated player IDs to prefer when merging")
	flag.Parse()

	prefSet := map[int]bool{}
	if *preferIDs != "" {
		for _, part := range strings.Split(*preferIDs, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.Atoi(part)
			if err != nil {
				exitErr(fmt.Sprintf("invalid prefer-player-id: %s", part))
			}
			prefSet[id] = true
		}
	}

	reports, err := loadReports(*rootDir)
	if err != nil {
		exitErr(err.Error())
	}

	masters, mappings := buildMasters(reports, *toleranceMS)

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		exitErr(err.Error())
	}

	mappingPath := filepath.Join(*outDir, "merged_mapping.json")
	writeJSON(mappingPath, mappingOutput{
		ReportMappings: mappings,
		Masters:        masters,
	})

	for _, master := range masters {
		merged, err := mergeMasterEvents(*rootDir, master, prefSet)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[warn] %s merge failed: %v\n", master.MasterID, err)
			continue
		}
		outPath := filepath.Join(*outDir, fmt.Sprintf("%s_events.json", master.MasterID))
		writeJSON(outPath, merged)
		fmt.Printf("merged: %s (%d events)\n", outPath, len(merged))
	}
}

func loadReports(rootDir string) ([]fightsResponse, error) {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return nil, err
	}

	var reports []fightsResponse
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		reportPath := filepath.Join(rootDir, entry.Name(), "report_fights.json")
		data, err := os.ReadFile(reportPath)
		if err != nil {
			continue
		}
		var report fightsResponse
		if err := json.Unmarshal(data, &report); err != nil {
			return nil, fmt.Errorf("parse %s: %w", reportPath, err)
		}
		reports = append(reports, report)
	}

	if len(reports) == 0 {
		return nil, fmt.Errorf("no report_fights.json found under %s", rootDir)
	}
	return reports, nil
}

func buildMasters(reports []fightsResponse, toleranceMS int64) ([]masterFight, map[string]map[string]string) {
	var masters []masterFight
	mappings := map[string]map[string]string{}
	masterSeq := 0

	for _, report := range reports {
		if mappings[report.Code] == nil {
			mappings[report.Code] = map[string]string{}
		}

		for _, fight := range report.Fights {
			absStart := report.Start + fight.StartTime
			duration := fight.EndTime - fight.StartTime
			encounterID := fight.EncounterID

			source := sourceFight{
				ReportCode:  report.Code,
				FightID:     fight.ID,
				AbsStart:    absStart,
				DurationMS:  duration,
				EncounterID: encounterID,
				FightName:   fight.Name,
			}

			masterIdx := findMaster(masters, encounterID, absStart, duration, toleranceMS)
			if masterIdx == -1 {
				masterSeq++
				masterID := fmt.Sprintf("D-f%d", masterSeq)
				masters = append(masters, masterFight{
					MasterID:    masterID,
					EncounterID: encounterID,
					Name:        nameFromEncounter(encounterID),
					Start:       absStart,
					DurationMS:  duration,
					Sources:     []sourceFight{source},
				})
				mappings[report.Code][strconv.Itoa(fight.ID)] = masterID
				continue
			}

			masters[masterIdx].Sources = append(masters[masterIdx].Sources, source)
			mappings[report.Code][strconv.Itoa(fight.ID)] = masters[masterIdx].MasterID
		}
	}

	return masters, mappings
}

func findMaster(masters []masterFight, encounterID int, absStart, duration, tolerance int64) int {
	for i, master := range masters {
		if master.EncounterID != encounterID {
			continue
		}
		if absDiff(master.Start, absStart) <= tolerance && absDiff(master.DurationMS, duration) <= tolerance {
			return i
		}
	}
	return -1
}

func mergeMasterEvents(rootDir string, master masterFight, prefSet map[int]bool) ([]map[string]interface{}, error) {
	var sources []sourceEvents
	for _, src := range master.Sources {
		eventsPath := filepath.Join(rootDir, src.ReportCode, fmt.Sprintf("fight_%d_events.json", src.FightID))
		data, err := os.ReadFile(eventsPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", eventsPath, err)
		}
		var events []map[string]interface{}
		if err := json.Unmarshal(data, &events); err != nil {
			return nil, fmt.Errorf("parse %s: %w", eventsPath, err)
		}

		se := sourceEvents{Source: src, Events: events, HasPref: false}
		if len(prefSet) > 0 {
			for _, ev := range events {
				sid := getInt(ev, "sourceID")
				tid := getInt(ev, "targetID")
				if prefSet[sid] || prefSet[tid] {
					se.HasPref = true
					break
				}
			}
		}
		sources = append(sources, se)
	}

	if len(prefSet) > 0 {
		sort.SliceStable(sources, func(i, j int) bool {
			if sources[i].HasPref == sources[j].HasPref {
				return sources[i].Source.ReportCode < sources[j].Source.ReportCode
			}
			return sources[i].HasPref && !sources[j].HasPref
		})
	}

	seen := map[string]bool{}
	merged := make([]map[string]interface{}, 0)
	for _, src := range sources {
		for _, ev := range src.Events {
			key := eventKey(ev, master.Start)
			if seen[key] {
				continue
			}
			seen[key] = true

			ev["_source_report"] = src.Source.ReportCode
			ev["_source_fight"] = src.Source.FightID
			ev["_master_id"] = master.MasterID

			merged = append(merged, ev)
		}
	}

	sort.SliceStable(merged, func(i, j int) bool {
		return getInt64(merged[i], "timestamp") < getInt64(merged[j], "timestamp")
	})

	return merged, nil
}

func eventKey(ev map[string]interface{}, fightStart int64) string {
	timestamp := getInt64(ev, "timestamp")
	sid := getInt(ev, "sourceID")
	tid := getInt(ev, "targetID")
	typeName := getString(ev, "type")
	abilityGUID := getAbilityGUID(ev)
	packetID := getInt(ev, "packetID")
	amount := getInt64(ev, "amount")

	return fmt.Sprintf("%d|%d|%d|%s|%d|%d|%d|%d", fightStart, timestamp, packetID, typeName, sid, tid, abilityGUID, amount)
}

func getAbilityGUID(ev map[string]interface{}) int {
	ability, ok := ev["ability"].(map[string]interface{})
	if !ok {
		return 0
	}
	if guid, ok := ability["guid"].(float64); ok {
		return int(guid)
	}
	return 0
}

func getInt(ev map[string]interface{}, key string) int {
	v, ok := ev[key]
	if !ok {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		parsed, _ := strconv.Atoi(t)
		return parsed
	default:
		return 0
	}
}

func getInt64(ev map[string]interface{}, key string) int64 {
	v, ok := ev[key]
	if !ok {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case string:
		parsed, _ := strconv.ParseInt(t, 10, 64)
		return parsed
	default:
		return 0
	}
}

func getString(ev map[string]interface{}, key string) string {
	v, ok := ev[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func nameFromEncounter(encounterID int) string {
	return fmt.Sprintf("encounter_%d", encounterID)
}

func absDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

func writeJSON(path string, data interface{}) {
	file, err := os.Create(path)
	if err != nil {
		exitErr(err.Error())
	}
	defer file.Close()

	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		exitErr(err.Error())
	}
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
