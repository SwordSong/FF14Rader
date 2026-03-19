# FFLogs V2 API Reference for FFXIV Analysis

## Dashboard Link
https://www.fflogs.com/v2/api-docs

## Common Queries

### 1. Fetching Metadata & Actors
```graphql
query ($name: String, $server: String, $region: String) {
  characterData {
    character(name: $name, serverSlug: $server, serverRegion: $region) {
      recentReports(limit: 40) {
        data {
          code
          title
          startTime
          masterData {
            actors(type: "Player") {
              id
              name
              subType
            }
          }
          fights {
            id
            name
            kill
            difficulty # 101: Savage, 100: Ultimate
          }
        }
      }
    }
  }
}
```

### 2. Fetching Table Data (Deaths, Debuffs, Avoidable Damage)
```graphql
query ($code: String, $fightIDs: [Int]) {
  reportData {
    report(code: $code) {
      # Deaths Table
      deaths: table(dataType: Deaths, fightIDs: $fightIDs)
      # Debuffs Table (Vulnerability stacks)
      debuffs: table(dataType: Debuffs, fightIDs: $fightIDs)
      # DamageTaken Table with filter for avoidable damage
      avoidable: table(dataType: DamageTaken, fightIDs: $fightIDs, filterExpression: "ability.damageIsAvoidable = true")
    }
  }
}
```

## JSON Structure (Table API)

### Debuffs Table Example
The `count` field in `entries` represents the application count.
```json
{
  "entries": [
    {
      "id": 15,
      "name": "Player Name",
      "count": 3, 
      "stacks": 5
    }
  ]
}
```

### DamageTaken Table Example
Used to count times a player was hit by avoidable mechanics.
```json
{
  "entries": [
    {
      "id": 15,
      "name": "Player Name",
      "count": 2, # Number of hits
      "total": 150000 # Total damage
    }
  ]
}
```
