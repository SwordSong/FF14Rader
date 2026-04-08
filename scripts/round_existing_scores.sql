-- 将 fight_sync_maps 历史评分字段统一四舍五入到两位小数（PostgreSQL）
-- 用法：psql "$DATABASE_URL" -f scripts/round_existing_scores.sql

BEGIN;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'fight_sync_maps'
    ) THEN
        UPDATE fight_sync_maps
        SET
            checklist_abs = ROUND(checklist_abs::numeric, 2)::double precision,
            checklist_confidence = ROUND(checklist_confidence::numeric, 2)::double precision,
            checklist_adj = ROUND(checklist_adj::numeric, 2)::double precision,
            suggestion_penalty = ROUND(suggestion_penalty::numeric, 2)::double precision,
            utility_score = ROUND(utility_score::numeric, 2)::double precision,
            survival_penalty = ROUND(survival_penalty::numeric, 2)::double precision,
            job_module_score = ROUND(job_module_score::numeric, 2)::double precision,
            battle_score = ROUND(battle_score::numeric, 2)::double precision,
            fight_weight = ROUND(fight_weight::numeric, 2)::double precision,
            weighted_battle_score = ROUND(weighted_battle_score::numeric, 2)::double precision
        WHERE
            checklist_abs IS NOT NULL
            OR checklist_confidence IS NOT NULL
            OR checklist_adj IS NOT NULL
            OR suggestion_penalty IS NOT NULL
            OR utility_score IS NOT NULL
            OR survival_penalty IS NOT NULL
            OR job_module_score IS NOT NULL
            OR battle_score IS NOT NULL
            OR fight_weight IS NOT NULL
            OR weighted_battle_score IS NOT NULL;
    END IF;
END
$$;

COMMIT;
