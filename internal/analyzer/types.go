package analyzer

// AnalysisFight holds the per-fight fields needed for performance aggregation.
// It should be populated from fight_sync_maps plus fight_cache joins.
type AnalysisFight struct {
	Kill            bool
	Percentile      float64
	Deaths          int
	VulnStacks      int
	AvoidableDamage int
	Duration        int
	StartTime       int64
	Name            string
	Job             string
}
