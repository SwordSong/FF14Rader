package render

import (
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/fogleman/gg"
	"github.com/user/ff14rader/internal/models"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

// RadarChart 负责绘制 9 维度雷达图
type RadarChart struct {
	Width  int
	Height int
}

func NewRadarChart(w, h int) *RadarChart {
	return &RadarChart{Width: w, Height: h}
}

// DrawMetrics 为任意维度评分绘制雷达图并保存。
func (r *RadarChart) DrawMetrics(title string, labels []string, values []float64, outputPath string) error {
	if len(labels) == 0 || len(labels) != len(values) {
		return fmt.Errorf("labels/values length mismatch")
	}

	dc := gg.NewContext(r.Width, r.Height)

	// 设置背景色
	dc.SetRGB(1, 1, 1)
	dc.Clear()

	loadFontFaceWithFallback(dc, 16)

	centerX := float64(r.Width) / 2
	centerY := float64(r.Height) / 2
	radius := math.Min(centerX, centerY) * 0.62

	if strings.TrimSpace(title) != "" {
		dc.SetRGB(0.08, 0.15, 0.25)
		dc.DrawStringAnchored(title, centerX, 28, 0.5, 0.5)
	}

	numPoints := len(labels)
	angleStep := 2 * math.Pi / float64(numPoints)

	// 绘制背景网格
	dc.SetRGBA(0.8, 0.8, 0.8, 0.5)
	dc.SetLineWidth(1)
	for i := 1; i <= 5; i++ {
		currentRadius := radius * float64(i) / 5
		for j := 0; j < numPoints; j++ {
			angle := float64(j)*angleStep - math.Pi/2
			x := centerX + currentRadius*math.Cos(angle)
			y := centerY + currentRadius*math.Sin(angle)
			if j == 0 {
				dc.MoveTo(x, y)
			} else {
				dc.LineTo(x, y)
			}
		}
		dc.ClosePath()
		dc.Stroke()
	}

	for i := 0; i < numPoints; i++ {
		angle := float64(i)*angleStep - math.Pi/2

		dc.SetRGBA(0.8, 0.8, 0.8, 0.5)
		dc.MoveTo(centerX, centerY)
		x := centerX + radius*math.Cos(angle)
		y := centerY + radius*math.Sin(angle)
		dc.LineTo(x, y)
		dc.Stroke()

		labelX := centerX + (radius+42)*math.Cos(angle)
		labelY := centerY + (radius+42)*math.Sin(angle)
		dc.SetRGB(0.0, 50/255.0, 100/255.0)
		dc.DrawStringAnchored(labels[i], labelX, labelY, 0.5, 0.5)

		scoreStr := fmt.Sprintf("%.1f", clampMetric(values[i]))
		dc.SetRGBA(0.4, 0.4, 0.4, 0.85)
		dc.DrawStringAnchored(scoreStr, labelX, labelY+18, 0.5, 0.5)
	}

	// 绘制数据区域
	dc.SetRGBA(0.2, 0.6, 1.0, 0.35)
	for i := 0; i < numPoints; i++ {
		angle := float64(i)*angleStep - math.Pi/2
		valRadius := radius * (clampMetric(values[i]) / 100.0)
		x := centerX + valRadius*math.Cos(angle)
		y := centerY + valRadius*math.Sin(angle)
		if i == 0 {
			dc.MoveTo(x, y)
		} else {
			dc.LineTo(x, y)
		}
	}
	dc.ClosePath()
	dc.FillPreserve()

	dc.SetRGB(0.1, 0.4, 0.8)
	dc.SetLineWidth(2)
	dc.Stroke()

	return dc.SavePNG(outputPath)
}

// Draw 为指定的性能数据绘制雷达图并保存
func (r *RadarChart) Draw(perf *models.Performance, outputPath string) error {
	labels := []string{
		"输出能力", "爆发利用", "技能覆盖",
		"团队贡献", "生存能力", "稳定度",
		"开荒表现", "机制处理", "潜力值",
	}
	values := []float64{
		perf.Output, perf.Burst, perf.Uptime,
		perf.Utility, perf.Survivability, perf.Consistency,
		perf.Progression, perf.Mechanics, perf.Potential,
	}
	return r.DrawMetrics("九维评分", labels, values, outputPath)
}

func clampMetric(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func loadFontFaceWithFallback(dc *gg.Context, size float64) {
	paths := []string{
		"/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/truetype/noto/NotoSansSC-Regular.otf",
		"/usr/share/fonts/opentype/noto/NotoSansSC-Regular.otf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"C:\\Windows\\Fonts\\msyh.ttc",
		"C:\\Windows\\Fonts\\arial.ttf",
	}
	for _, path := range paths {
		if err := loadFontFaceCompat(dc, path, size); err == nil {
			return
		}
	}
}

func loadFontFaceCompat(dc *gg.Context, path string, size float64) error {
	if err := dc.LoadFontFace(path, size); err == nil {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	collection, err := opentype.ParseCollection(data)
	if err != nil {
		return err
	}
	if collection.NumFonts() == 0 {
		return fmt.Errorf("empty font collection: %s", path)
	}

	fontInCollection, err := collection.Font(0)
	if err != nil {
		return err
	}

	face, err := opentype.NewFace(fontInCollection, &opentype.FaceOptions{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return err
	}

	dc.SetFontFace(face)
	return nil
}
