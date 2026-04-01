package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type fightsResponse struct {
	Code            string        `json:"code"`
	Start           int64         `json:"start"`
	End             int64         `json:"end"`
	Title           string        `json:"title"`
	Fights          []fightInfo   `json:"fights"`
	FriendlyPlayers []interface{} `json:"friendlyPlayers"`
}

type fightInfo struct {
	ID               int     `json:"id"`
	Name             string  `json:"name"`
	Kill             bool    `json:"kill"`
	Difficulty       int     `json:"difficulty"`
	EncounterID      int     `json:"encounterID"`
	StartTime        int64   `json:"start_time"`
	EndTime          int64   `json:"end_time"`
	BossPercentage   float64 `json:"bossPercentage"`
	FightPercentage  float64 `json:"fightPercentage"`
	AverageItemLevel float64 `json:"averageItemLevel"`
	Size             int     `json:"size"`
}

type eventsResponse struct {
	Events            []map[string]interface{} `json:"events"`
	NextPageTimestamp *int64                   `json:"nextPageTimestamp"`
}

type savedEventsPayload struct {
	Events []map[string]interface{} `json:"events"`
	Count  int                      `json:"count"`
}

type fightSummary struct {
	ID               int     `json:"id"`
	Name             string  `json:"name"`
	Kill             bool    `json:"kill"`
	Difficulty       int     `json:"difficulty"`
	EncounterID      int     `json:"encounterID"`
	StartTime        int64   `json:"start_time"`
	EndTime          int64   `json:"end_time"`
	DurationSeconds  int64   `json:"duration_seconds"`
	BossPercentage   float64 `json:"bossPercentage"`
	FightPercentage  float64 `json:"fightPercentage"`
	AverageItemLevel float64 `json:"averageItemLevel"`
	Size             int     `json:"size"`
	EventCount       int     `json:"event_count"`
	TopEventTypes    []kv    `json:"top_event_types"`
	TopAbilities     []kv    `json:"top_abilities"`
}

type kv struct {
	Key   string `json:"key"`
	Value int    `json:"value"`
}

type reportSummary struct {
	Code       string         `json:"code"`
	Title      string         `json:"title"`
	Start      int64          `json:"start"`
	End        int64          `json:"end"`
	FightCount int            `json:"fight_count"`
	Fights     []fightSummary `json:"fights"`
}

func main() {
	code := flag.String("code", "", "FFLogs report code")
	outDir := flag.String("out", "./downloads/fflogs", "output directory")
	baseURL := flag.String("base-url", "https://www.fflogs.com/v1/", "FFLogs v1 API base url")
	apiKey := flag.String("api-key", "", "FFLogs v1 API key (optional; prefer FFLOGS_V1_API_KEY env)")
	limitFights := flag.Int("limit-fights", 0, "limit number of fights to download (0 = all)")
	flag.Parse()

	loadEnvFile(".env")

	if *code == "" {
		exitErr("missing -code")
	}

	key := *apiKey
	if key == "" {
		key = os.Getenv("FFLOGS_V1_API_KEY")
	}
	if key == "" {
		exitErr("missing FFLOGS_V1_API_KEY env or -api-key")
	}

	client := &http.Client{Timeout: 60 * time.Second}

	fights, err := fetchFights(client, *baseURL, key, *code)
	if err != nil {
		exitErr(err.Error())
	}

	if len(fights.Fights) == 0 {
		exitErr("no fights in report")
	}

	selected := fights.Fights
	if *limitFights > 0 && *limitFights < len(selected) {
		selected = selected[:*limitFights]
	}

	outputRoot := filepath.Join(*outDir, *code)
	if err := os.MkdirAll(outputRoot, 0755); err != nil {
		exitErr(err.Error())
	}

	writeJSON(filepath.Join(outputRoot, "report_fights.json"), fights)

	summary := reportSummary{
		Code:       fights.Code,
		Title:      fights.Title,
		Start:      fights.Start,
		End:        fights.End,
		FightCount: len(selected),
	}

	for _, fight := range selected {
		events, err := fetchAllEvents(client, *baseURL, key, *code, fight)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[warn] fight %d events fetch failed: %v\n", fight.ID, err)
			continue
		}

		eventsPath := filepath.Join(outputRoot, fmt.Sprintf("fight_%d_events.json", fight.ID))
		writeJSON(eventsPath, toSavedEventsPayload(events))

		fightSummary := summarizeFight(fight, events)
		summary.Fights = append(summary.Fights, fightSummary)
	}

	writeJSON(filepath.Join(outputRoot, "report_summary.json"), summary)
}

func toSavedEventsPayload(events []map[string]interface{}) savedEventsPayload {
	return savedEventsPayload{
		Events: events,
		Count:  len(events),
	}
}

func fetchFights(client *http.Client, baseURL, apiKey, code string) (*fightsResponse, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/report/fights/" + code

	q := u.Query()
	q.Set("api_key", apiKey)
	u.RawQuery = q.Encode()

	resp, err := client.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fights request failed: %s: %s", resp.Status, truncate(string(body), 256))
	}

	var parsed fightsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	return &parsed, nil
}

func fetchAllEvents(client *http.Client, baseURL, apiKey, code string, fight fightInfo) ([]map[string]interface{}, error) {
	if fight.EndTime <= fight.StartTime {
		return nil, errors.New("invalid fight time range")
	}

	start := fight.StartTime
	end := fight.EndTime
	all := make([]map[string]interface{}, 0)

	for {
		data, err := fetchEventsPage(client, baseURL, apiKey, code, start, end)
		if err != nil {
			return nil, err
		}

		all = append(all, data.Events...)
		if data.NextPageTimestamp == nil || len(data.Events) == 0 {
			break
		}
		start = *data.NextPageTimestamp
	}

	return all, nil
}

func fetchEventsPage(client *http.Client, baseURL, apiKey, code string, start, end int64) (*eventsResponse, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/report/events/" + code

	q := u.Query()
	q.Set("api_key", apiKey)
	q.Set("start", fmt.Sprintf("%d", start))
	q.Set("end", fmt.Sprintf("%d", end))
	q.Set("translate", "true")
	u.RawQuery = q.Encode()

	resp, err := client.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("events request failed: %s: %s", resp.Status, truncate(string(body), 256))
	}

	var parsed eventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	return &parsed, nil
}

func summarizeFight(fight fightInfo, events []map[string]interface{}) fightSummary {
	typeCounts := map[string]int{}
	abilityCounts := map[string]int{}

	for _, ev := range events {
		if t, ok := ev["type"].(string); ok {
			typeCounts[t]++
		}

		if ability, ok := ev["ability"].(map[string]interface{}); ok {
			if name, ok := ability["name"].(string); ok {
				abilityCounts[name]++
			}
		}
	}

	return fightSummary{
		ID:               fight.ID,
		Name:             fight.Name,
		Kill:             fight.Kill,
		Difficulty:       fight.Difficulty,
		EncounterID:      fight.EncounterID,
		StartTime:        fight.StartTime,
		EndTime:          fight.EndTime,
		DurationSeconds:  (fight.EndTime - fight.StartTime) / 1000,
		BossPercentage:   fight.BossPercentage,
		FightPercentage:  fight.FightPercentage,
		AverageItemLevel: fight.AverageItemLevel,
		Size:             fight.Size,
		EventCount:       len(events),
		TopEventTypes:    topN(typeCounts, 5),
		TopAbilities:     topN(abilityCounts, 5),
	}
}

func topN(counts map[string]int, n int) []kv {
	list := make([]kv, 0, len(counts))
	for k, v := range counts {
		list = append(list, kv{Key: k, Value: v})
	}
	if len(list) == 0 {
		return list
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].Value == list[j].Value {
			return list[i].Key < list[j].Key
		}
		return list[i].Value > list[j].Value
	})

	if len(list) > n {
		list = list[:n]
	}
	return list
}

func writeJSON(path string, value interface{}) {
	file, err := os.Create(path)
	if err != nil {
		exitErr(err.Error())
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		exitErr(err.Error())
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, "\"\"")
		if key == "" {
			continue
		}
		if existing, exists := os.LookupEnv(key); exists && existing != "" {
			continue
		}
		_ = os.Setenv(key, val)
	}
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
