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
	}, &models.Player{}, &models.Report{}, &models.FightSyncMap{}))
	if err != nil {
		log.Fatalf("无法注册读写分离插件 (dbresolver): %v", err)
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
			WHERE table_name = 'fight_sync_maps' AND column_name = 'start_time'
		)
		OR EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'fight_sync_maps' AND column_name = 'end_time'
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
			id, master_id, source_ids, player_id, timestamp,
			fight_id, kill, job, downloaded, downloaded_at, parsed_done,
			start_time, end_time, name, boss_percentage
		)
		SELECT
			id,
			master_id,
			source_ids,
			player_id,
			timestamp,
			fight_id,
			kill,
			job,
			COALESCE(downloaded, false),
			downloaded_at,
			COALESCE(parsed_done, false),
			start_time,
			end_time,
			boss_name,
			wipe_progress
		FROM fight_sync_maps;

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
		WHERE table_name = 'fight_sync_maps'
	) THEN
		UPDATE fight_sync_maps
		SET downloaded = false
		WHERE downloaded IS NULL;
		ALTER TABLE fight_sync_maps ALTER COLUMN downloaded SET NOT NULL;
	END IF;
END
$$;`)
	err = DB.AutoMigrate(&models.Player{}, &models.Report{}, &models.FightSyncMap{})
	if err != nil {
		log.Fatalf("数据库迁移失败: %v", err)
	}
	log.Println("数据库表结构迁移成功。")
}
