package scoring

import (
	"math"
	"testing"

	"github.com/user/ff14rader/internal/models"
)

func TestPartySignature_NormalizesAndSorts(t *testing.T) {
	sig := partySignature([]string{"  Alice", "bob", "alice", "BOB  ", ""})
	if sig != "alice|alice|bob|bob" {
		t.Fatalf("unexpected signature: %s", sig)
	}
}

func TestComputeEncounterProgressionScore_FirstPullKill(t *testing.T) {
	score, weight := computeEncounterProgressionScore([]models.FightSyncMap{
		{
			ID:              1,
			Timestamp:       100,
			Kill:            true,
			FightPercentage: 0,
			FriendPlayers:   []string{"a", "b", "c", "d", "e", "f", "g", "h"},
		},
	})

	if math.Abs(score-100) > 0.001 {
		t.Fatalf("expected score=100, got %.4f", score)
	}
	if math.Abs(weight-1) > 0.001 {
		t.Fatalf("expected weight=1, got %.4f", weight)
	}
}

func TestComputeEncounterProgressionScore_TeamChangeCompensates(t *testing.T) {
	sameTeam := []models.FightSyncMap{
		{ID: 1, Timestamp: 1, FightPercentage: 80, FriendPlayers: []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
		{ID: 2, Timestamp: 2, FightPercentage: 70, FriendPlayers: []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
		{ID: 3, Timestamp: 3, FightPercentage: 60, FriendPlayers: []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
		{ID: 4, Timestamp: 4, FightPercentage: 50, FriendPlayers: []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
		{ID: 5, Timestamp: 5, Kill: true, FriendPlayers: []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
	}

	teamChange := []models.FightSyncMap{
		{ID: 1, Timestamp: 1, FightPercentage: 80, FriendPlayers: []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
		{ID: 2, Timestamp: 2, FightPercentage: 70, FriendPlayers: []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
		{ID: 3, Timestamp: 3, FightPercentage: 60, FriendPlayers: []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
		{ID: 4, Timestamp: 4, FightPercentage: 20, FriendPlayers: []string{"i", "j", "k", "l", "m", "n", "o", "p"}},
		{ID: 5, Timestamp: 5, Kill: true, FriendPlayers: []string{"i", "j", "k", "l", "m", "n", "o", "p"}},
	}

	scoreSame, _ := computeEncounterProgressionScore(sameTeam)
	scoreChange, _ := computeEncounterProgressionScore(teamChange)

	if scoreChange <= scoreSame {
		t.Fatalf("expected team-change compensation score %.4f > same-team score %.4f", scoreChange, scoreSame)
	}
}

func TestComputeEncounterProgressionScore_NoKillHasLowerWeight(t *testing.T) {
	score, weight := computeEncounterProgressionScore([]models.FightSyncMap{
		{ID: 1, Timestamp: 1, FightPercentage: 95, FriendPlayers: []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
		{ID: 2, Timestamp: 2, FightPercentage: 92, FriendPlayers: []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
		{ID: 3, Timestamp: 3, FightPercentage: 90, FriendPlayers: []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
	})

	if score >= 70 {
		t.Fatalf("expected no-kill score to stay lower, got %.4f", score)
	}
	if !(weight > 0 && weight < 1) {
		t.Fatalf("expected weight in (0,1), got %.4f", weight)
	}
}
