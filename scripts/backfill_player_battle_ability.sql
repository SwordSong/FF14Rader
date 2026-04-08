-- 回填 players.battle_ability（基于全部已评分战斗，1-100）
-- 用法：psql "$DATABASE_URL" -f scripts/backfill_player_battle_ability.sql

BEGIN;

WITH agg AS (
    SELECT
        f.player_id,
        COALESCE(SUM(f.weighted_battle_score), 0) AS weighted_sum,
        COALESCE(SUM(f.fight_weight), 0) AS total_weight
    FROM fight_sync_maps f
    WHERE f.scored_at IS NOT NULL
      AND f.fight_weight > 0
      AND f.weighted_battle_score IS NOT NULL
    GROUP BY f.player_id
),
calc AS (
    SELECT
        a.player_id,
        CASE
            WHEN a.total_weight <= 0 THEN 0::double precision
            ELSE GREATEST(1.0, LEAST(100.0, ROUND((a.weighted_sum / a.total_weight)::numeric, 2)::double precision))
        END AS battle_ability
    FROM agg a
)
UPDATE players p
SET battle_ability = c.battle_ability,
    updated_at = now()
FROM calc c
WHERE p.id = c.player_id;

COMMIT;
