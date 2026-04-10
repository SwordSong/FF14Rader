package cluster

import (
	"sort"
	"sync"
	"time"
)

// UnparsedCodeDict 维护“用户未解析code字典”：playerID -> reportCode -> fightIDs。
type UnparsedCodeDict struct {
	mu        sync.RWMutex
	data      map[uint]map[string][]int
	updatedAt time.Time
}

var globalUnparsedCodeDict = NewUnparsedCodeDict()

func NewUnparsedCodeDict() *UnparsedCodeDict {
	return &UnparsedCodeDict{data: make(map[uint]map[string][]int)}
}

func GlobalUnparsedCodeDict() *UnparsedCodeDict {
	return globalUnparsedCodeDict
}

func normalizeFightIDs(ids []int) []int {
	if len(ids) == 0 {
		return nil
	}
	set := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		set[id] = struct{}{}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]int, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func normalizeCodeFightMap(input map[string][]int) map[string][]int {
	if len(input) == 0 {
		return map[string][]int{}
	}
	out := make(map[string][]int, len(input))
	for rawCode, fightIDs := range input {
		code := NormalizeReportCode(rawCode)
		if code == "" {
			continue
		}
		normalized := normalizeFightIDs(fightIDs)
		if existing, ok := out[code]; ok {
			out[code] = normalizeFightIDs(append(existing, normalized...))
			continue
		}
		out[code] = normalized
	}
	return out
}

func deepCopyDict(input map[uint]map[string][]int) map[uint]map[string][]int {
	out := make(map[uint]map[string][]int, len(input))
	for playerID, codeMap := range input {
		child := make(map[string][]int, len(codeMap))
		for code, fights := range codeMap {
			child[code] = append([]int(nil), fights...)
		}
		out[playerID] = child
	}
	return out
}

// ReplaceAll 用数据库重建结果整体替换字典。
func (d *UnparsedCodeDict) ReplaceAll(all map[uint]map[string][]int) {
	if d == nil {
		return
	}
	next := make(map[uint]map[string][]int, len(all))
	for playerID, codeMap := range all {
		if playerID == 0 {
			continue
		}
		normalized := normalizeCodeFightMap(codeMap)
		if len(normalized) == 0 {
			continue
		}
		next[playerID] = normalized
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.data = next
	d.updatedAt = time.Now()
}

// UpsertPlayer 增量写入某个玩家的未解析 code/fightID。
func (d *UnparsedCodeDict) UpsertPlayer(playerID uint, codeMap map[string][]int) (int, int) {
	if d == nil || playerID == 0 {
		return 0, 0
	}
	normalized := normalizeCodeFightMap(codeMap)
	if len(normalized) == 0 {
		return 0, 0
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.data == nil {
		d.data = make(map[uint]map[string][]int)
	}
	if _, ok := d.data[playerID]; !ok {
		d.data[playerID] = make(map[string][]int)
	}

	codeCount := 0
	fightCount := 0
	for code, fightIDs := range normalized {
		existing := d.data[playerID][code]
		merged := normalizeFightIDs(append(existing, fightIDs...))
		d.data[playerID][code] = merged
		codeCount++
		fightCount += len(merged)
	}
	d.updatedAt = time.Now()
	return codeCount, fightCount
}

func (d *UnparsedCodeDict) Snapshot() map[uint]map[string][]int {
	if d == nil {
		return map[uint]map[string][]int{}
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.data) == 0 {
		return map[uint]map[string][]int{}
	}
	return deepCopyDict(d.data)
}

func (d *UnparsedCodeDict) Stats() (int, int, int, time.Time) {
	if d == nil {
		return 0, 0, 0, time.Time{}
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	players := len(d.data)
	codes := 0
	fights := 0
	for _, codeMap := range d.data {
		codes += len(codeMap)
		for _, ids := range codeMap {
			fights += len(ids)
		}
	}
	return players, codes, fights, d.updatedAt
}
