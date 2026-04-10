package db

import (
	"log"

	"gorm.io/gorm/logger"

	"github.com/user/ff14rader/internal/models"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/plugin/dbresolver"
)

var DB *gorm.DB

const currentDBSchemaVersion = "2026-04-11-v14"

func InitDB(writeDSN, readDSN string) {
	var err error

	// 1. 先连接 Master 库 (写库)
	DB, err = gorm.Open(postgres.Open(writeDSN), &gorm.Config{
		PrepareStmt: false, // 禁用全局预编译语句，解决某些 PostgreSQL 环境下的 prepared statement name is already in use 错误
		Logger:      logger.Default.LogMode(logger.Error),
	})
	if err != nil {
		log.Fatalf("无法连接数据库 (Master): %v", err)
	}

	// 2. 配置读写分离 (dbresolver)
	// 使用 dbresolver 插件，读操作将分发给 readDSN
	err = DB.Use(dbresolver.Register(dbresolver.Config{
		Sources:  []gorm.Dialector{postgres.Open(writeDSN)},
		Replicas: []gorm.Dialector{postgres.Open(readDSN)},
		Policy:   dbresolver.RandomPolicy{}, // 随机读策略
	}, &models.Player{}, &models.Report{}, &models.FightSyncMap{}, &models.RadarSyncTask{}, &models.ClusterHostEndpoint{}, &models.ClusterReportHost{}))
	if err != nil {
		log.Fatalf("无法注册读写分离插件 (dbresolver): %v", err)
	}

	if shouldSkip, reason := shouldSkipMigrations(DB); shouldSkip {
		log.Printf("跳过数据库迁移：%s", reason)
		return
	}

	// 3. 自动迁移表结构 (在 Master 执行)
	log.Println("正在 Master 库执行表结构迁移...")
	_ = DB.Exec(`DO $$
BEGIN
	IF EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_name = 'reports'
	) AND NOT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'reports' AND column_name = 'master_report'
	) THEN
		DROP TABLE reports;
	END IF;

	IF EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_name = 'report_parse_logs'
	) THEN
		CREATE TABLE IF NOT EXISTS reports_new (
			id bigserial PRIMARY KEY,
			player_id bigint NOT NULL,
			master_report varchar(50) NOT NULL,
			source_report varchar(50) NOT NULL,
			parsed_at timestamptz,
			parsed_done boolean DEFAULT false,
			downloaded boolean DEFAULT false,
			report_metadata jsonb,
			start_time bigint,
			end_time bigint,
			title text,
			created_at timestamptz
		);

		INSERT INTO reports_new (
			id, player_id, master_report, source_report,
			parsed_at, parsed_done, downloaded, report_metadata,
			created_at
		)
		SELECT
			id, player_id, master_report, source_report,
			parsed_at, parsed_done, COALESCE(downloaded, false), report_metadata,
			created_at
		FROM report_parse_logs;

		DROP TABLE report_parse_logs;
		ALTER TABLE reports_new RENAME TO reports;
		PERFORM setval(pg_get_serial_sequence('reports', 'id'), COALESCE((SELECT MAX(id) FROM reports), 1));
	END IF;

	IF NOT EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_name = 'reports'
	) THEN
		CREATE TABLE reports (
			id bigserial PRIMARY KEY,
			player_id bigint NOT NULL,
			master_report varchar(50) NOT NULL,
			source_report varchar(50) NOT NULL,
			parsed_at timestamptz,
			parsed_done boolean DEFAULT false,
			downloaded boolean DEFAULT false,
			report_metadata jsonb,
			start_time bigint,
			end_time bigint,
			title text,
			created_at timestamptz
		);
	END IF;
END
$$;`)
	_ = DB.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_reports_player_source ON reports (player_id, source_report);`)
	_ = DB.Exec(`DROP TABLE IF EXISTS fight_caches;`)
	_ = DB.Exec(`DROP TABLE IF EXISTS fight_scores;`)
	_ = DB.Exec(`DROP TABLE IF EXISTS performances;`)
	// 先尝试将旧的 text[] source_ids 转为 jsonb，避免自动迁移失败
	_ = DB.Exec(`DO $$
BEGIN
	IF EXISTS (
		SELECT 1
		FROM pg_attribute a
		JOIN pg_class c ON a.attrelid = c.oid
		JOIN pg_type t ON a.atttypid = t.oid
		WHERE c.relname = 'fight_sync_maps'
			AND a.attname = 'source_ids'
			AND t.typname = '_text'
	) THEN
		ALTER TABLE fight_sync_maps
			ALTER COLUMN source_ids TYPE jsonb
			USING to_jsonb(source_ids);
	END IF;
END
$$;`)
	_ = DB.Exec(`DO $$
BEGIN
	IF EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_name = 'fight_sync_maps'
	) THEN
		IF NOT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'downloaded'
		) THEN
			ALTER TABLE fight_sync_maps ADD COLUMN downloaded boolean DEFAULT false;
		END IF;
	END IF;
END
$$;`)
	_ = DB.Exec(`DO $$
BEGIN
	IF EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_name = 'fight_sync_maps'
	) THEN
		IF NOT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'downloaded_at'
		) THEN
			ALTER TABLE fight_sync_maps ADD COLUMN downloaded_at timestamptz;
		END IF;
	END IF;
END
$$;`)
	_ = DB.Exec(`DO $$
BEGIN
	IF EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_name = 'fight_sync_maps'
	) THEN
		IF NOT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'parsed_done'
		) THEN
			ALTER TABLE fight_sync_maps ADD COLUMN parsed_done boolean DEFAULT false;
		END IF;
	END IF;
END
$$;`)
	_ = DB.Exec(`DO $$
BEGIN
	IF EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_name = 'fight_sync_maps'
	) AND (
		NOT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'name'
		)
		OR NOT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'boss_percentage'
		)
		OR NOT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'fight_percentage'
		)
		OR NOT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'floor'
		)
		OR NOT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'game_zone'
		)
		OR NOT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'difficulty'
		)
		OR NOT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'encounter_id'
		)
		OR EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'boss_name'
		)
		OR EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'wipe_progress'
		)
		OR EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'title'
		)
		OR EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'duration'
		)
	) THEN
		CREATE TABLE IF NOT EXISTS fight_sync_maps_new (
			id bigserial PRIMARY KEY,
			master_id varchar(100),
			source_ids jsonb,
			friendplayers jsonb,
			friendplayers_usable boolean DEFAULT false,
			player_id bigint,
			timestamp bigint,
			fight_id integer,
			kill boolean,
			job text,
			downloaded boolean DEFAULT false,
			downloaded_at timestamptz,
			parsed_done boolean DEFAULT false,
			start_time bigint,
			end_time bigint,
			name varchar(100),
			boss_percentage double precision,
			fight_percentage double precision,
			floor varchar(20),
			game_zone jsonb,
			difficulty integer,
			encounter_id integer
		);

		INSERT INTO fight_sync_maps_new (
			id, master_id, source_ids, friendplayers, friendplayers_usable, player_id, timestamp,
			fight_id, kill, job, downloaded, downloaded_at, parsed_done,
			start_time, end_time, name, boss_percentage,
			fight_percentage, floor, game_zone, difficulty, encounter_id
		)
		SELECT
			src.id,
			COALESCE(NULLIF(to_jsonb(src)->>'master_id', ''), src.id::text),
			COALESCE(to_jsonb(src)->'source_ids', '[]'::jsonb),
			COALESCE(to_jsonb(src)->'friendplayers', to_jsonb(src)->'friend_players', '[]'::jsonb),
			COALESCE(NULLIF(to_jsonb(src)->>'friendplayers_usable', '')::boolean, NULLIF(to_jsonb(src)->>'friend_players_usable', '')::boolean, false),
			NULLIF(to_jsonb(src)->>'player_id', '')::bigint,
			COALESCE(
				NULLIF(to_jsonb(src)->>'timestamp', '')::bigint,
				NULLIF(to_jsonb(src)->>'start_time', '')::bigint,
				0
			),
			COALESCE(NULLIF(to_jsonb(src)->>'fight_id', '')::integer, 0),
			COALESCE(NULLIF(to_jsonb(src)->>'kill', '')::boolean, false),
			COALESCE(to_jsonb(src)->>'job', ''),
			COALESCE(NULLIF(to_jsonb(src)->>'downloaded', '')::boolean, false),
			NULLIF(to_jsonb(src)->>'downloaded_at', '')::timestamptz,
			COALESCE(NULLIF(to_jsonb(src)->>'parsed_done', '')::boolean, false),
			COALESCE(NULLIF(to_jsonb(src)->>'start_time', '')::bigint, 0),
			COALESCE(
				NULLIF(to_jsonb(src)->>'end_time', '')::bigint,
				COALESCE(NULLIF(to_jsonb(src)->>'start_time', '')::bigint, 0)
				+ COALESCE(NULLIF(to_jsonb(src)->>'duration', '')::bigint, 0)
			),
			COALESCE(
				NULLIF(to_jsonb(src)->>'name', ''),
				NULLIF(to_jsonb(src)->>'boss_name', ''),
				NULLIF(to_jsonb(src)->>'title', ''),
				''
			),
			COALESCE(
				NULLIF(to_jsonb(src)->>'boss_percentage', '')::double precision,
				NULLIF(to_jsonb(src)->>'wipe_progress', '')::double precision,
				0
			),
			COALESCE(NULLIF(to_jsonb(src)->>'fight_percentage', '')::double precision, 0),
			COALESCE(NULLIF(to_jsonb(src)->>'floor', ''), ''),
			COALESCE(to_jsonb(src)->'game_zone', '{}'::jsonb),
			COALESCE(NULLIF(to_jsonb(src)->>'difficulty', '')::integer, 0),
			COALESCE(NULLIF(to_jsonb(src)->>'encounter_id', '')::integer, 0)
		FROM fight_sync_maps src;

		DROP TABLE fight_sync_maps;
		ALTER TABLE fight_sync_maps_new RENAME TO fight_sync_maps;
		PERFORM setval(pg_get_serial_sequence('fight_sync_maps', 'id'), COALESCE((SELECT MAX(id) FROM fight_sync_maps), 1));
	END IF;
END
$$;`)
	_ = DB.Exec(`DO $$
BEGIN
	IF EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_name = 'players'
	) THEN
		ALTER TABLE players DROP COLUMN IF EXISTS output_percentile_sum;
		ALTER TABLE players DROP COLUMN IF EXISTS output_percentile_count;
		ALTER TABLE players DROP COLUMN IF EXISTS execution_percentile_sum;
		ALTER TABLE players DROP COLUMN IF EXISTS execution_percentile_count;
	END IF;

	IF EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_name = 'fight_sync_maps'
	) THEN
		ALTER TABLE fight_sync_maps DROP COLUMN IF EXISTS percentile_aggregated;
		ALTER TABLE fight_sync_maps DROP COLUMN IF EXISTS output_percentile;
		ALTER TABLE fight_sync_maps DROP COLUMN IF EXISTS execution_percentile;
	END IF;
END
$$;`)
	_ = DB.Exec(`DO $$
BEGIN
	IF EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_name = 'fight_sync_maps'
	) THEN
		UPDATE fight_sync_maps
		SET downloaded = false
		WHERE downloaded IS NULL;
		ALTER TABLE fight_sync_maps ALTER COLUMN downloaded SET NOT NULL;
	END IF;
END
$$;`)
	_ = DB.Exec(`DO $$
BEGIN
	IF EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'players' AND column_name = 'all_reports_code'
	) THEN
		ALTER TABLE players DROP COLUMN all_reports_code;
	END IF;
END
$$;`)
	err = DB.AutoMigrate(&models.Player{}, &models.Report{}, &models.FightSyncMap{}, &models.RadarSyncTask{}, &models.ClusterHostEndpoint{}, &models.ClusterReportHost{})
	if err != nil {
		log.Fatalf("数据库迁移失败: %v", err)
	}
	if err := setSchemaVersion(DB, currentDBSchemaVersion); err != nil {
		log.Printf("[WARN] 写入数据库 schema 版本失败: %v", err)
	}
	log.Println("数据库表结构迁移成功。")
}

func shouldSkipMigrations(db *gorm.DB) (bool, string) {
	if err := ensureSchemaMetaTable(db); err != nil {
		log.Printf("[WARN] 初始化 schema_meta 失败，将使用结构探测: %v", err)
	} else {
		version, ok, err := getSchemaVersion(db)
		if err != nil {
			log.Printf("[WARN] 读取 schema 版本失败，将使用结构探测: %v", err)
		} else if ok && version == currentDBSchemaVersion {
			return true, "schema 版本已匹配"
		}
	}

	if schemaLooksCurrent(db) {
		if err := setSchemaVersion(db, currentDBSchemaVersion); err != nil {
			log.Printf("[WARN] 回填 schema 版本失败: %v", err)
		}
		return true, "已满足当前表结构要求"
	}

	return false, "需要执行迁移"
}

func ensureSchemaMetaTable(db *gorm.DB) error {
	return db.Exec(`CREATE TABLE IF NOT EXISTS schema_meta (
		meta_key varchar(100) PRIMARY KEY,
		meta_value text NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now()
	);`).Error
}

func getSchemaVersion(db *gorm.DB) (string, bool, error) {
	type schemaMetaRow struct {
		MetaValue string
	}
	var row schemaMetaRow
	tx := db.Raw("SELECT meta_value FROM schema_meta WHERE meta_key = ?", "db_schema_version").Scan(&row)
	if tx.Error != nil {
		return "", false, tx.Error
	}
	if tx.RowsAffected == 0 {
		return "", false, nil
	}
	return row.MetaValue, true, nil
}

func setSchemaVersion(db *gorm.DB, version string) error {
	return db.Exec(`
		INSERT INTO schema_meta (meta_key, meta_value, updated_at)
		VALUES ('db_schema_version', ?, now())
		ON CONFLICT (meta_key)
		DO UPDATE SET meta_value = EXCLUDED.meta_value, updated_at = now();
	`, version).Error
}

func schemaLooksCurrent(db *gorm.DB) bool {
	requiredTables := []string{"players", "reports", "fight_sync_maps", "radar_sync_tasks", "cluster_host_endpoints", "cluster_report_hosts"}
	for _, table := range requiredTables {
		if !tableExists(db, table) {
			return false
		}
	}

	requiredColumns := map[string][]string{
		"players": {
			"id", "name", "server", "region", "race", "gender", "lodestone_id", "common_job", "all_report_codes",
			"output_ability", "battle_ability", "team_contribution", "progression_speed",
			"stability_score", "mechanics_score", "potential_score", "pichash", "pic_updated_at",
			"created_at", "updated_at",
		},
		"reports": {
			"id", "player_id", "master_report", "source_report", "parsed_done", "downloaded",
			"report_metadata", "start_time", "end_time", "title", "created_at",
		},
		"fight_sync_maps": {
			"id", "master_id", "source_ids", "friendplayers", "friendplayers_usable", "player_id", "timestamp", "fight_id", "kill", "job",
			"downloaded", "downloaded_at", "parsed_done", "start_time", "end_time", "name",
			"boss_percentage", "fight_percentage", "floor", "game_zone", "difficulty", "encounter_id",
			"score_actor_name", "checklist_abs", "checklist_confidence", "checklist_adj", "suggestion_penalty",
			"utility_score", "survival_penalty", "job_module_score", "battle_score", "fight_weight",
			"weighted_battle_score", "raw_module_metrics", "scored_at",
		},
		"radar_sync_tasks": {
			"id", "task_key", "username", "server", "region", "status", "last_error", "retry_count",
			"requested_at", "started_at", "finished_at", "created_at", "updated_at",
		},
		"cluster_host_endpoints": {
			"id", "host", "control_endpoint", "last_seen_at", "created_at", "updated_at",
		},
		"cluster_report_hosts": {
			"id", "report_code", "host", "last_assigned_at", "created_at", "updated_at",
		},
	}

	for table, cols := range requiredColumns {
		for _, col := range cols {
			if !columnExists(db, table, col) {
				return false
			}
		}
	}

	return indexExists(db, "reports", "idx_reports_player_source")
}

func tableExists(db *gorm.DB, tableName string) bool {
	var exists bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = ?
		)
	`, tableName).Scan(&exists).Error; err != nil {
		return false
	}
	return exists
}

func columnExists(db *gorm.DB, tableName, columnName string) bool {
	var exists bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = ? AND column_name = ?
		)
	`, tableName, columnName).Scan(&exists).Error; err != nil {
		return false
	}
	return exists
}

func indexExists(db *gorm.DB, tableName, indexName string) bool {
	var exists bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_indexes
			WHERE schemaname = 'public' AND tablename = ? AND indexname = ?
		)
	`, tableName, indexName).Scan(&exists).Error; err != nil {
		return false
	}
	return exists
}
