-- 为 players / reports / fight_sync_maps 添加中文表备注与字段备注（PostgreSQL）
-- 用法：psql "$DATABASE_URL" -f scripts/add_db_comments.sql

BEGIN;

DO $$
DECLARE
    rec RECORD;
BEGIN
    -- 表备注
    FOR rec IN
        SELECT * FROM (VALUES
            ('players', '玩家基础信息表'),
            ('reports', '报告映射与解析状态表（按 source_report 存储）'),
            ('fight_sync_maps', '战斗同步映射与评分结果表')
        ) AS t(table_name, comment_text)
    LOOP
        IF EXISTS (
            SELECT 1
            FROM information_schema.tables
            WHERE table_schema = 'public'
              AND table_name = rec.table_name
        ) THEN
            EXECUTE format('COMMENT ON TABLE %I IS %L', rec.table_name, rec.comment_text);
        END IF;
    END LOOP;

    -- 字段备注
    FOR rec IN
        SELECT * FROM (VALUES
            -- players
            ('players', 'id', '主键ID'),
            ('players', 'name', '玩家名称'),
            ('players', 'server', '服务器名'),
            ('players', 'region', '大区标识'),
            ('players', 'all_report_codes', '该玩家关联的全部报告编号列表（JSON数组）'),
            ('players', 'output_ability', '输出能力评分（1-100）'),
            ('players', 'battle_ability', '个人战斗能力总评分（1-100，按全部已评分战斗加权汇总）'),
            ('players', 'team_contribution', '团队贡献评分（1-100）'),
            ('players', 'progression_speed', '开荒速度评分（1-100）'),
            ('players', 'stability_score', '稳定度评分（1-100）'),
            ('players', 'mechanics_score', '机制处理评分（1-100）'),
            ('players', 'potential_score', '潜力值评分（1-100）'),
            ('players', 'created_at', '创建时间'),
            ('players', 'updated_at', '更新时间'),

            -- reports
            ('reports', 'id', '主键ID'),
            ('reports', 'player_id', '玩家ID（关联 players.id）'),
            ('reports', 'master_report', '主报告编号（去重后的主键报告）'),
            ('reports', 'source_report', '源报告编号（原始报告）'),
            ('reports', 'parsed_at', '解析时间'),
            ('reports', 'parsed_done', '是否解析完成'),
            ('reports', 'downloaded', '是否已下载事件数据'),
            ('reports', 'report_metadata', '报告元数据（含 masterData 等结构）'),
            ('reports', 'start_time', '报告开始时间（毫秒时间戳）'),
            ('reports', 'end_time', '报告结束时间（毫秒时间戳）'),
            ('reports', 'title', '报告标题'),
            ('reports', 'created_at', '创建时间'),

            -- fight_sync_maps
            ('fight_sync_maps', 'id', '主键ID'),
            ('fight_sync_maps', 'master_id', '主战斗ID（通常为 reportCode-fightID）'),
            ('fight_sync_maps', 'source_ids', '来源战斗ID列表（JSON数组）'),
            ('fight_sync_maps', 'friendplayers', '队友名单（含自己，通常共8人，JSON数组）'),
            ('fight_sync_maps', 'friendplayers_usable', '队友名单是否可用（名单人数=8时为true）'),
            ('fight_sync_maps', 'player_id', '玩家ID（关联 players.id）'),
            ('fight_sync_maps', 'timestamp', '战斗开始时间戳（秒）'),
            ('fight_sync_maps', 'fight_id', '战斗ID（报告内编号）'),
            ('fight_sync_maps', 'kill', '是否击杀成功'),
            ('fight_sync_maps', 'job', '职业标识'),
            ('fight_sync_maps', 'downloaded', '该战斗事件是否已下载'),
            ('fight_sync_maps', 'downloaded_at', '事件下载完成时间'),
            ('fight_sync_maps', 'parsed_done', '是否已解析完成'),
            ('fight_sync_maps', 'start_time', '战斗开始时间（相对报告毫秒）'),
            ('fight_sync_maps', 'end_time', '战斗结束时间（相对报告毫秒）'),
            ('fight_sync_maps', 'name', '战斗名称'),
            ('fight_sync_maps', 'boss_percentage', 'Boss剩余血量百分比'),
            ('fight_sync_maps', 'fight_percentage', '战斗进度百分比（FFLogs字段）'),
            ('fight_sync_maps', 'floor', '楼层/分层标签'),
            ('fight_sync_maps', 'game_zone', '副本区域信息（JSON）'),
            ('fight_sync_maps', 'difficulty', '难度ID'),
            ('fight_sync_maps', 'encounter_id', '遭遇战ID'),
            ('fight_sync_maps', 'score_actor_name', '用于评分的角色名'),
            ('fight_sync_maps', 'checklist_abs', '清单绝对分'),
            ('fight_sync_maps', 'checklist_confidence', '清单可信度'),
            ('fight_sync_maps', 'checklist_adj', '清单修正分'),
            ('fight_sync_maps', 'suggestion_penalty', '建议项惩罚分（越高越差）'),
            ('fight_sync_maps', 'utility_score', '团队贡献分'),
            ('fight_sync_maps', 'survival_penalty', '生存惩罚分（越高越差）'),
            ('fight_sync_maps', 'job_module_score', '职业模块分'),
            ('fight_sync_maps', 'battle_score', '战斗分'),
            ('fight_sync_maps', 'fight_weight', '战斗权重'),
            ('fight_sync_maps', 'weighted_battle_score', '加权战斗分'),
            ('fight_sync_maps', 'raw_module_metrics', '原始模块指标（JSON）'),
            ('fight_sync_maps', 'scored_at', '评分时间')
        ) AS c(table_name, column_name, comment_text)
    LOOP
        IF EXISTS (
            SELECT 1
            FROM information_schema.columns
            WHERE table_schema = 'public'
              AND table_name = rec.table_name
              AND column_name = rec.column_name
        ) THEN
            EXECUTE format('COMMENT ON COLUMN %I.%I IS %L', rec.table_name, rec.column_name, rec.comment_text);
        END IF;
    END LOOP;
END
$$;

COMMIT;
