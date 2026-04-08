-- 回填 fight_sync_maps.friendplayers_usable
-- 规则：队友名单（friendplayers）人数等于 8 才可用于开荒速度分析
-- 用法：psql "$DATABASE_URL" -f scripts/backfill_friendplayers_usable.sql

BEGIN;

UPDATE fight_sync_maps
SET friendplayers_usable = (
    CASE
        WHEN jsonb_typeof(COALESCE(friendplayers, '[]'::jsonb)) = 'array'
             AND jsonb_array_length(COALESCE(friendplayers, '[]'::jsonb)) = 8
        THEN true
        ELSE false
    END
);

COMMIT;
