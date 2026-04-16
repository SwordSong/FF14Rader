package client

import (
	"log"

	internalapi "github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/cluster"
	clusterclient "github.com/user/ff14rader/internal/cluster/client"
)

// StartWorker 启动工作协程。
func StartWorker() {
	fflogsClient := *internalapi.NewFFLogsClient()
	syncManager := *internalapi.NewSyncManager(&fflogsClient)

	localHost := cluster.LocalHost()
	scanDir := cluster.ReportsScanDir()
	reports, seedErr := cluster.CollectReportCodesFromDir(scanDir)
	if seedErr != nil {
		log.Printf("[CLUSTER] 本地 reports 扫描失败 host=%s dir=%s err=%v", localHost, scanDir, seedErr)
	} else {
		log.Printf("[CLUSTER] 本地 reports 扫描完成 host=%s dir=%s reports=%d", localHost, scanDir, len(reports))
	}

	clusterclient.StartAutoRegisterAndHeartbeat()
	clusterclient.StartClusterTaskPullLoop(&syncManager)
}
