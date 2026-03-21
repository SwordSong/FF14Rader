package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
)

type fightPayload struct {
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

type batchRequest struct {
	Fights []fightPayload `json:"fights"`
}

type fightResult struct {
	ID      string          `json:"id"`
	Status  string          `json:"status"`
	Error   string          `json:"error,omitempty"`
	Summary json.RawMessage `json:"summary,omitempty"`
}

type batchResponse struct {
	Results []fightResult `json:"results"`
}

// Minimal end-to-end smoke test using a long-running HTTP xivanalysis service:
// 1) ensure player exists
// 2) optional sync from FFLogs (with events already cached in FightCache)
// 3) fetch latest fights for the player
// 4) send fights in batches to the xivanalysis HTTP service (POST /analyze)
func main() {
	name := flag.String("name", "世无仙", "character name")
	server := flag.String("server", "延夏", "server name")
	region := flag.String("region", "CN", "server region")
	limit := flag.Int("limit", 1, "number of latest fights to analyze")
	skipSync := flag.Bool("skip-sync", false, "skip FFLogs sync (use existing DB data)")
	apiURL := flag.String("api-url", "http://localhost:3000/analyze", "xivanalysis API endpoint")
	batchSize := flag.Int("batch-size", 10, "max fights per batch request")
	clientTimeout := flag.Duration("http-timeout", 45*time.Second, "HTTP client timeout per request")
	printSummary := flag.Bool("print-summary", true, "pretty print per-fight summary returned by analyzer")
	flag.Parse()

	cfg := config.LoadConfig()
	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	ctx := context.Background()

	// ensure player exists
	var player models.Player
	if err := db.DB.Where("name = ? AND server = ? AND region = ?", *name, *server, *region).First(&player).Error; err != nil {
		player = models.Player{Name: *name, Server: *server, Region: *region}
		if err := db.DB.Create(&player).Error; err != nil {
			log.Fatalf("create player failed: %v", err)
		}
	}

	fflogsClient := api.NewFFLogsClient(cfg.FFLogsClientID, cfg.FFLogsClientSecret)

	if !*skipSync {
		sm := api.NewSyncManager(fflogsClient)
		if err := sm.StartIncrementalSync(ctx, player.ID, player.Name, player.Server, player.Region); err != nil {
			log.Fatalf("sync failed: %v", err)
		}
	}

	var reports []models.Report
	if err := db.DB.Where("player_id = ?", player.ID).Order("start_time desc").Limit(*limit).Find(&reports).Error; err != nil {
		log.Fatalf("query reports failed: %v", err)
	}
	if len(reports) == 0 {
		log.Fatalf("no reports found for player %s-%s", player.Name, player.Server)
	}

	fights := make([]fightPayload, 0, len(reports))
	for _, r := range reports {
		var cache models.FightCache
		if err := db.DB.Where("id = ?", r.ID).First(&cache).Error; err != nil {
			log.Fatalf("fight cache not found for %s: %v", r.ID, err)
		}

		// cache.Data already contains combined tables + events JSON
		fights = append(fights, fightPayload{ID: cache.ID, Data: json.RawMessage(cache.Data)})
	}

	if *batchSize <= 0 {
		*batchSize = 1
	}

	httpClient := &http.Client{Timeout: *clientTimeout}
	batches := chunkPayloads(fights, *batchSize)
	log.Printf("sending %d fights in %d batch(es) to %s", len(fights), len(batches), *apiURL)

	sent := 0
	succeeded := 0
	skipped := 0

	for i, batch := range batches {
		if len(batch) == 0 {
			continue
		}

		start := time.Now()
		bodyBytes, err := json.Marshal(batchRequest{Fights: batch})
		if err != nil {
			log.Fatalf("marshal batch %d failed: %v", i+1, err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, *apiURL, bytes.NewReader(bodyBytes))
		if err != nil {
			log.Fatalf("create request failed: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			log.Fatalf("HTTP request failed (batch %d): %v", i+1, err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("batch %d returned %s, skipping %d fights; body: %s", i+1, resp.Status, len(batch), truncate(string(respBody), 512))
			skipped += len(batch)
			sent += len(batch)
			continue
		}

		var parsed batchResponse
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			log.Fatalf("decode response failed (batch %d): %v", i+1, err)
		}

		if len(parsed.Results) == 0 {
			log.Printf("batch %d returned empty results; marking %d fights skipped", i+1, len(batch))
			skipped += len(batch)
			sent += len(batch)
			continue
		}

		for _, res := range parsed.Results {
			sent++
			status := strings.ToLower(res.Status)
			if status == "ok" || status == "success" {
				succeeded++
				if *printSummary {
					log.Printf("fight %s: ok; summary:\n%s", res.ID, prettyJSON(res.Summary))
				} else {
					log.Printf("fight %s: ok; summary: %s", res.ID, truncate(string(res.Summary), 512))
				}
			} else {
				skipped++
				log.Printf("fight %s: skipped (%s) %s", res.ID, res.Status, truncate(res.Error, 512))
			}
		}

		log.Printf("batch %d finished in %s", i+1, time.Since(start))
	}

	// Final JSON summary
	out := map[string]interface{}{
		"player":    player.Name,
		"server":    player.Server,
		"region":    player.Region,
		"requested": len(fights),
		"sent":      sent,
		"succeeded": succeeded,
		"skipped":   skipped,
	}
	enc := json.NewEncoder(log.Writer())
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func chunkPayloads(fights []fightPayload, size int) [][]fightPayload {
	if size <= 0 {
		size = 1
	}

	var batches [][]fightPayload
	for i := 0; i < len(fights); i += size {
		end := i + size
		if end > len(fights) {
			end = len(fights)
		}
		batches = append(batches, fights[i:end])
	}
	return batches
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func prettyJSON(raw []byte) string {
	if len(raw) == 0 {
		return "(empty)"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
