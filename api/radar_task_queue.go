package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	radarTaskStatusPending = "pending"
	radarTaskStatusRunning = "running"
	radarTaskStatusDone    = "done"
	radarTaskStatusFailed  = "failed"
)

func enqueueRadarSyncTask(username, server, region string) (*models.RadarSyncTask, bool, error) {
	name := strings.TrimSpace(username)
	world := strings.TrimSpace(server)
	area := strings.TrimSpace(region)
	if area == "" {
		area = "CN"
	}
	taskKey := radarTaskKey(name, world)
	if taskKey == "" {
		return nil, false, fmt.Errorf("invalid task key")
	}

	now := time.Now()
	created := false
	var task models.RadarSyncTask

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("task_key = ?", taskKey).
			First(&task).Error
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}

			task = models.RadarSyncTask{
				TaskKey:     taskKey,
				Username:    name,
				Server:      world,
				Region:      area,
				Status:      radarTaskStatusPending,
				RequestedAt: now,
			}
			if createErr := tx.Create(&task).Error; createErr != nil {
				return createErr
			}
			created = true
			return nil
		}

		updates := map[string]interface{}{
			"username":     name,
			"server":       world,
			"region":       area,
			"requested_at": now,
		}
		if task.Status != radarTaskStatusRunning {
			updates["status"] = radarTaskStatusPending
			updates["last_error"] = ""
			updates["started_at"] = gorm.Expr("NULL")
			updates["finished_at"] = gorm.Expr("NULL")
		}

		if updateErr := tx.Model(&models.RadarSyncTask{}).
			Where("id = ?", task.ID).
			Updates(updates).Error; updateErr != nil {
			return updateErr
		}

		return tx.First(&task, task.ID).Error
	})
	if err != nil {
		return nil, false, err
	}
	return &task, created, nil
}

func getRadarSyncTaskByTaskKey(taskKey string) (*models.RadarSyncTask, error) {
	key := strings.TrimSpace(taskKey)
	if key == "" {
		return nil, nil
	}

	var task models.RadarSyncTask
	err := db.DB.Where("task_key = ?", key).First(&task).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &task, nil
}

func claimNextRadarSyncTask() (*models.RadarSyncTask, error) {
	var task models.RadarSyncTask
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", radarTaskStatusPending).
			Order("requested_at ASC").
			First(&task).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		now := time.Now()
		if err := tx.Model(&models.RadarSyncTask{}).
			Where("id = ?", task.ID).
			Updates(map[string]interface{}{
				"status":     radarTaskStatusRunning,
				"started_at": now,
			}).Error; err != nil {
			return err
		}

		return tx.First(&task, task.ID).Error
	})
	if err != nil {
		return nil, err
	}
	if task.ID == 0 {
		return nil, nil
	}
	return &task, nil
}

func finishRadarSyncTask(taskID uint, execErr error) {
	if taskID == 0 {
		return
	}

	now := time.Now()
	updates := map[string]interface{}{
		"finished_at": now,
	}

	if execErr == nil {
		updates["status"] = radarTaskStatusDone
		updates["last_error"] = ""
	} else {
		updates["status"] = radarTaskStatusFailed
		updates["last_error"] = strings.TrimSpace(execErr.Error())
		updates["retry_count"] = gorm.Expr("retry_count + ?", 1)
	}

	if err := db.DB.Model(&models.RadarSyncTask{}).
		Where("id = ?", taskID).
		Updates(updates).Error; err != nil {
		log.Printf("[RADAR] finish task update failed id=%d err=%v", taskID, err)
	}
}

func radarWorkerInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("RADAR_TASK_WORKER_INTERVAL_SEC"))
	if raw == "" {
		return 2 * time.Second
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 2 * time.Second
	}
	return time.Duration(n) * time.Second
}

func (s *Service) StartRadarTaskWorker() {
	if s == nil || s.SyncManager == nil {
		return
	}

	interval := radarWorkerInterval()
	s.radarWorkerOnce.Do(func() {
		go func() {
			log.Printf("[RADAR] async worker started interval=%s", interval)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for {
				s.consumeOneRadarSyncTask()
				<-ticker.C
			}
		}()
	})
}

func (s *Service) consumeOneRadarSyncTask() {
	if s == nil || s.SyncManager == nil {
		return
	}

	task, err := claimNextRadarSyncTask()
	if err != nil {
		log.Printf("[RADAR] claim task failed: %v", err)
		return
	}
	if task == nil {
		return
	}

	log.Printf("[RADAR] task start id=%d key=%s user=%s server=%s", task.ID, task.TaskKey, task.Username, task.Server)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	execErr := s.SyncManager.StartIncrementalSync(ctx, task.Username, task.Server, task.Region)
	if execErr == nil {
		var row *playerRadarSnapshot
		row, execErr = loadPlayerRadarSnapshot(task.Username, task.Server)
		if execErr == nil {
			if row == nil {
				execErr = fmt.Errorf("player not found after sync")
			} else {
				_, _, execErr = generateAndPersistPlayerRadar(row)
			}
		}
	}

	finishRadarSyncTask(task.ID, execErr)
	if execErr != nil {
		log.Printf("[RADAR] task failed id=%d key=%s err=%v", task.ID, task.TaskKey, execErr)
		return
	}
	log.Printf("[RADAR] task done id=%d key=%s", task.ID, task.TaskKey)
}
