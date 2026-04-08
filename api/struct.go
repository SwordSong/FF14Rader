package api

import (
	"time"
)

// 定义接口返回的数据结构
type MarketDataResponse struct {
	Status     string          `json:"status"`
	Data       []MarketDataRow `json:"data"`
	Time       string          `json:"currentTime"`
	TotalCount int64           `json:"totalCount"`
}

type AveragePriceResponse struct {
	Status     string            `json:"status"`
	ItemId     int64             `json:"itemId"`
	Data       []AveragePriceRow `json:"data"`
	Time       string            `json:"currentTime"`
	TotalCount int64             `json:"totalCount"`
}

type DashboardStatisponse struct {
	Status     string            `json:"status"`
	Data       []DashboardStatis `json:"data"`
	Time       string            `json:"currentTime"`
	TotalCount int64             `json:"totalCount"`
}

type KlineStatisponse struct {
	Status     string        `json:"status"`
	Data       []KlineStatis `json:"data"`
	Time       string        `json:"currentTime"`
	TotalCount int64         `json:"totalCount"`
}
type ErroStatusResponse struct {
	Status string `json:"status"`
	Text   string `json:"text"`
	Time   string `json:"currentTime"`
}

// 客户端查询字段
type ClientQuery struct {
	Server              int64    `json:"server"`
	PropsClassification []string `json:"propsClassification"`
	StartDate           string   `json:"startDate"`
	EndDate             string   `json:"endDate"`
	MinQuantity         int64    `json:"minQuantity"`
	MaxQuantity         int64    `json:"maxQuantity"`
	MaxPrice            int64    `json:"maxPrice"`
	MinPrice            int64    `json:"minPrice"`
	ItemName            string   `json:"itemName"`
	Limit               int64    `json:"limit"`
	Page                int64    `json:"page"`
}

type MarketDataRow struct {
	// --- 9 个 mds.* 字段 ---
	StatDate         time.Time // mds.stat_date (使用 time.Time 接收 DATE)
	ItemID           int32     // mds.item_id
	WorldID          int64     // mds.world_id
	HQ               bool      // mds.hq
	TotalSalesAmount int64     // mds.total_sales_amount
	TotalQuantity    int64     // mds.total_quantity
	AmountRank       int16     //mds.aoumt_rank
	QuantityRank     int16     //mds.quantity_rank
	AveragePrice     int64     // mds.average_price (或 decimal.Decimal)
	// --- 2 个 JOIN 来的字段 ---
	ItemName           string // im.item_name
	ItemClassification string // im.item_classification
	TotalCount         int64  // Total_count
}

type AveragePriceRow struct {
	HQ          bool  `json:"hq"`
	Server      int64 `json:"server"`
	LowestPrice int64 `json:"lowest_price"`
}

type DashboardStatisQuery struct {
	ItemId int64 `json:"itemId"`
	Server int64 `json:"server"`
}

type DashboardStatis struct {
	Server               int64   // ts2.server
	ItemId               int64   // ts2.item_id
	LastSellingPrice     int64   // ts2.last_selling_price
	AveragePrice         float32 // ts2.average_price
	AveragePriceIncrease float64 // ts2.average_price_increase
	TradeCount24hour     int64   // ts2.trade_count_24hour
	ToalAmount24hour     int64   // ts2.total_amount_24hour
	HighPrice            int64   // ts2.high_price
	LowPrice             int64   // ts2.low_price
	Liquidity            int64   // ts2.liquidity
	Volatility           float64 // ts2.volatility
	MarketTradeCount     *int64  // ts2.market_trade_count
	MarkTotalAmount      *int64  // ts2.mark_total_amount
	HQ                   bool    // ts2.hq
	TradeVolume24hour    int64   // ts2.trade_volume_24hour
}

type KlineStatisQuery struct {
	ItemId    int64     `json:"itemId"`
	Server    int64     `json:"server"`
	HQ        bool      `json:"hq"`
	Interval  int64     `json:"interval"`
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`
}

type KlineStatis struct {
	Bucket_time   time.Time // ts2.bucket_time
	Average_price float32   // ts2.average_price
	Trade_count   int64     // ts2.trade_count
	Total_volume  int64     // ts2.total_volume
	Final_price   int64     // ts2.final_price

}

var ServerList = map[string]struct{}{
	"拉诺西亚": {},
	"幻影群岛": {},
	"神意之地": {},
	"萌芽池":  {},
	"红玉海":  {},
	"宇宙和音": {},
	"沃仙曦染": {},
	"晨曦王座": {},
	"潮风亭":  {},
	"神拳痕":  {},
	"白银乡":  {},
	"白金幻象": {},
	"旅人栈桥": {},
	"拂晓之间": {},
	"龙巢神殿": {},
	"梦羽宝境": {},
	"紫水栈桥": {},
	"延夏":   {},
	"静语庄园": {},
	"摩杜纳":  {},
	"海猫茶屋": {},
	"柔风海湾": {},
	"琥珀原":  {},
	"水晶塔":  {},
	"银泪湖":  {},
	"太阳海岸": {},
	"伊修加德": {},
	"红茶川":  {}}
