-- 基于全部已评分战斗评估个人战斗能力（1-100）
-- 用法：psql "$DATABASE_URL" -f scripts/evaluate_personal_battle_score.sql

WITH scored AS (
    SELECT
        f.player_id,
        f.battle_score,
        f.fight_weight,
        f.weighted_battle_score,
        f.scored_at
    FROM fight_sync_maps f
    WHERE f.scored_at IS NOT NULL
      AND f.fight_weight IS NOT NULL
      AND f.fight_weight > 0
      AND f.weighted_battle_score IS NOT NULL
),
agg AS (
    SELECT
        s.player_id,
        COUNT(*) AS fight_count,
        SUM(s.fight_weight) AS total_weight,
        SUM(s.weighted_battle_score) AS weighted_sum,
        AVG(s.battle_score) AS avg_battle_score,
        MAX(s.scored_at) AS last_scored_at
    FROM scored s
    GROUP BY s.player_id
),
base AS (
    SELECT
        a.player_id,
        a.fight_count,
        a.total_weight,
        a.weighted_sum,
        a.avg_battle_score,
        a.last_scored_at,
        (a.weighted_sum / NULLIF(a.total_weight, 0)) AS personal_battle_score_raw
    FROM agg a
),
with_conf AS (
    SELECT
        b.*,
        LEAST(1.0, b.total_weight / 8.0) AS sample_confidence
    FROM base b
)
SELECT
    p.id AS player_id,
    p.name AS player_name,
    p.server,
    c.fight_count,
    ROUND(c.total_weight::numeric, 2) AS total_weight,
    ROUND(c.avg_battle_score::numeric, 2) AS avg_single_fight_score,
    ROUND(LEAST(100.0, GREATEST(1.0, c.personal_battle_score_raw))::numeric, 2) AS personal_battle_score_100,
    ROUND(c.sample_confidence::numeric, 2) AS sample_confidence,
    ROUND(
        LEAST(
            100.0,
            GREATEST(
                1.0,
                c.sample_confidence * c.personal_battle_score_raw + (1.0 - c.sample_confidence) * 60.0
            )
        )::numeric,
        2
    ) AS personal_battle_score_stable_100,
    c.last_scored_at
FROM with_conf c
JOIN players p ON p.id = c.player_id
ORDER BY personal_battle_score_stable_100 DESC, c.fight_count DESC;
