package render

import (
	"fmt"
	"math"

	"github.com/fogleman/gg"
	"github.com/user/ff14rader/internal/models"
)

// RadarChart 负责绘制 9 维度雷达图
type RadarChart struct {
	Width  int
	Height int
}

func NewRadarChart(w, h int) *RadarChart {
	return &RadarChart{Width: w, Height: h}
}

// Draw 为指定的性能数据绘制雷达图并保存
func (r *RadarChart) Draw(perf *models.Performance, outputPath string) error {
	dc := gg.NewContext(r.Width, r.Height)

	// 设置背景色
	dc.SetRGB(1, 1, 1)
	dc.Clear()

	// 尝试加载中文字体 (微软雅黑) 以显示标签
	fontPath := "C:\\Windows\\Fonts\\msyh.ttc"
	if err := dc.LoadFontFace(fontPath, 16); err != nil {
		// 如果微软雅黑不存在，尝试宋体
		_ = dc.LoadFontFace("C:\\Windows\\Fonts\\simsun.ttc", 16)
	}

	centerX := float64(r.Width) / 2
	centerY := float64(r.Height) / 2
	radius := math.Min(centerX, centerY) * 0.7 // 缩小一点半径给文字留位置

	// 定义 9 个维度的标签
	labels := []string{
		"输出能力", "爆发利用", "技能覆盖",
		"团队贡献", "生存能力", "稳定度",
		"开荒表现", "机制处理", "潜力值",
	}

	// 对应 Performance 中的数值 (0-100)
	values := []float64{
		perf.Output, perf.Burst, perf.Uptime,
		perf.Utility, perf.Survivability, perf.Consistency,
		perf.Progression, perf.Mechanics, perf.Potential,
	}

	numPoints := len(labels)
	angleStep := 2 * math.Pi / float64(numPoints)

	// 1. 绘制背景网格 (蜘蛛网桩)
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

	// 2. 绘制轴线
	for i := 0; i < numPoints; i++ {
		angle := float64(i)*angleStep - math.Pi/2

		// 轴线 (使用亮灰色)
		dc.SetRGBA(0.8, 0.8, 0.8, 0.5)
		dc.MoveTo(centerX, centerY)
		x := centerX + radius*math.Cos(angle)
		y := centerY + radius*math.Sin(angle)
		dc.LineTo(x, y)
		dc.Stroke()

		// 绘制标签
		labelX := centerX + (radius+40)*math.Cos(angle)
		labelY := centerY + (radius+40)*math.Sin(angle)
		dc.SetRGB(0, 50/255.0, 100/255.0) // 深蓝色标签

		// 1. 绘制主要指标标签
		dc.DrawStringAnchored(labels[i], labelX, labelY, 0.5, 0.5)

		// 2. 绘制具体的分数 (保留1位小数)
		scoreStr := fmt.Sprintf("%.1f", values[i])
		dc.SetRGBA(0.4, 0.4, 0.4, 0.8)
		if err := dc.LoadFontFace("C:\\Windows\\Fonts\\arial.ttf", 12); err == nil {
			dc.DrawStringAnchored(scoreStr, labelX, labelY+20, 0.5, 0.5)
			// 恢复中文字体用于下一轮循环
			_ = dc.LoadFontFace("C:\\Windows\\Fonts\\msyh.ttc", 16)
		}
	}

	// 3. 绘制数据区域 (彩色填充)
	dc.SetRGBA(0.2, 0.6, 1.0, 0.4) // 浅蓝色半透明
	for i := 0; i < numPoints; i++ {
		angle := float64(i)*angleStep - math.Pi/2
		valRadius := radius * (values[i] / 100.0)
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

	// 4. 绘制轮廓线
	dc.SetRGB(0.1, 0.4, 0.8)
	dc.SetLineWidth(2)
	dc.Stroke()

	// 保存图片
	return dc.SavePNG(outputPath)
}
