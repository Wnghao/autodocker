package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/registry"
	"github.com/haihaipypy/autodocker/internal/scanner"
	"github.com/haihaipypy/autodocker/internal/store"
	"github.com/haihaipypy/autodocker/internal/updater"
)

// handleHealth 健康检查。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]any{
		"status":        "ok",
		"time":          time.Now().Format(time.RFC3339),
		"scanning":      s.sc.Busy(),
		"updating":      s.up.Busy(),
		"scan_interval": s.LoadSettings().ScanInterval,
	})
}

// handleStats 仪表盘统计。
// handleStats 首页那排数字。
//
// 口径是**容器**，不是镜像：用户想知道「我这些容器有几个该更新」，
// 而不是「磁盘上躺了多少层」。以前把全部镜像都算进去，总数 15、
// 有更新 + 可选更新 + 已是最新 却只有 8，对不上，看着像统计坏了。
// 本机镜像里本来就含没人用的悬空层和纯粹的远端监控项，它们不属于「监控中」。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	byStatus, err := s.st.CountImagesByStatus(model.OriginContainer)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "统计容器失败: "+err.Error())
		return
	}
	unread, err := s.st.CountUnread()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "统计未读失败: "+err.Error())
		return
	}
	var lastScan *model.Scan
	if sc, lerr := s.st.LastScan(); lerr == nil {
		lastScan = sc
	}
	total := 0
	for _, n := range byStatus {
		total += n
	}
	writeOK(w, map[string]any{
		"total":             total,
		"up_to_date":        byStatus[string(model.StatusUpToDate)],
		"update_available":  byStatus[string(model.StatusUpdatePending)],
		"new_version_tag":   byStatus[string(model.StatusNewTag)],
		"unknown":           byStatus[string(model.StatusUnknown)],
		"ignored":           byStatus[string(model.StatusIgnored)],
		"unread":            unread,
		"last_scan":         lastScan,
		"scanning":          s.sc.Busy(),
		"updating":          s.up.Busy(),
		"feishu_configured": s.nt.Feishu().Configured(),
		"feishu_longconn":   s.nt.LongConnRunning(),
		"auth_enabled":      s.AuthEnabled(),
	})
}

// handleListContainers 容器列表：一条记录对应一个容器。
func (s *Server) handleListContainers(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.st.ListImages(model.OriginContainer)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取容器列表失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"items": imgs, "total": len(imgs)})
}

// handleListImages 镜像列表：本机真实存在的镜像，加上只做远端盯梢的监控项。
//
// 与容器列表分开是有实在理由的：镜像是「占磁盘的东西」，关心用了多少空间、
// 能不能清掉、本地这份落后没有；容器是「正在跑的东西」，关心要不要重建。
// 早先把两者揉在同一个列表里，才会出现「这到底是镜像还是容器」的困惑。
func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	items, err := s.collectLocalImages(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取镜像列表失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"items": items, "total": len(items)})
}

// collectLocalImages 组装镜像列表：以 Docker 里真实存在的镜像为主，
// 叠上库里记的检测状态；库里还有、本机已经没有的，就是纯远端监控项。
func (s *Server) collectLocalImages(ctx context.Context) ([]model.LocalImage, error) {
	records, err := s.st.ListImages()
	if err != nil {
		return nil, err
	}
	// 同一个引用可能既有容器记录又有镜像记录，取「检测结果更有信息量」的那条。
	// 纯远端项本机没有镜像，进不了这张索引，它们在下面第二段单独列。
	byRef := map[string]*model.Image{}
	for i := range records {
		rec := &records[i]
		if rec.Origin == model.OriginWatch {
			continue
		}
		if prev, ok := byRef[rec.Reference]; !ok || statusRank(rec.Status) < statusRank(prev.Status) {
			byRef[rec.Reference] = rec
		}
	}

	// 哪些镜像正被容器使用 —— 既决定「使用中 / 未使用」，也决定能不能删。
	inUseID := map[string]bool{}
	usedBy := map[string][]string{}
	if containers, cerr := s.dk.ListContainers(ctx); cerr == nil {
		for _, c := range containers {
			if c.ImageID != "" {
				inUseID[c.ImageID] = true
			}
			if c.Image != "" {
				usedBy[c.Image] = append(usedBy[c.Image], c.Name)
			}
		}
	} else {
		s.log.Warn("列举容器失败，镜像的使用状态可能不准", "err", cerr)
	}

	out := []model.LocalImage{}
	seen := map[string]bool{}

	images, ierr := s.dk.ListImages(ctx)
	if ierr != nil {
		// Docker 读不到时退化成「只显示库里的记录」，总好过把整页拉白。
		s.log.Warn("列举本机镜像失败", "err", ierr)
	}
	for _, img := range images {
		tags := img.Tags
		dangling := len(tags) == 0
		if dangling {
			tags = []string{""}
		}
		for _, tag := range tags {
			row := model.LocalImage{
				ID: img.ID, Reference: tag,
				SizeBytes: img.Size, Created: img.Created, Dangling: dangling,
				InUsed: inUseID[img.ID],
			}
			key := tag
			if dangling {
				key = img.ID
				// 悬空镜像在 Docker 里没有 tag，`docker images` 也只显示 <none>；
				// 但它构建时写进配置的 OCI 标签还在，不少镜像会留着
				// org.opencontainers.image.title 或 source（源码仓库地址）。
				// 挖得到就拿来当名字，挖不到留空 —— 前端会退回显示短 ID，
				// 一屏好几个「未命名」照样分不清谁是谁。
				row.Name = labelImageName(img)
			} else {
				row.Name, row.Tag = splitImageTag(tag)
				if names := usedBy[tag]; len(names) > 0 {
					row.UsedBy = names
				}
			}
			if rec, ok := byRef[key]; ok {
				fillCheckState(&row, rec)
			}
			seen[key] = true
			out = append(out, row)
		}
	}

	// Docker 里没有对应镜像、库里却还留着的记录，也要列出来。
	// 它们有两种可能：WATCH 配的纯远端盯梢项，或者 Docker 暂时读不到 /
	// 镜像已被手工删掉但还没重新扫描。无论哪种，藏着都比显示出来更糟 ——
	// 页面会凭空少掉几行，用户根本不知道发生过什么。
	for i := range records {
		rec := &records[i]
		if rec.Origin == model.OriginContainer || seen[rec.Reference] {
			continue
		}
		seen[rec.Reference] = true
		row := model.LocalImage{ID: rec.Reference, Reference: rec.Reference}
		if rec.Origin == model.OriginLocal {
			if rec.ImageID != "" {
				row.ID = rec.ImageID
			}
			row.SizeBytes = rec.SizeBytes
			row.Created = rec.ImageCreated
		}
		fillCheckState(&row, rec)
		// 悬空镜像的 reference 本身就是一个镜像 ID，拆不出名字，也不该硬拆
		if !row.Dangling {
			row.Name, row.Tag = splitImageTag(rec.Reference)
		}
		out = append(out, row)
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := statusRank(a.Status), statusRank(b.Status); ra != rb {
			return ra < rb
		}
		// 未使用的排在前面：它们才是「清理镜像」要处理的对象
		if a.InUsed != b.InUsed {
			return !a.InUsed
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Tag < b.Tag
	})
	return out, nil
}

// fillCheckState 把库里的检测状态贴到镜像行上。
func fillCheckState(row *model.LocalImage, rec *model.Image) {
	row.Status = rec.Status
	row.LatestTag = rec.LatestTag
	row.LastError = rec.LastError
	row.MonitorID = rec.ID
	row.CheckMode = string(rec.Mode)
	row.RemoteOnly = rec.Origin == model.OriginWatch
	if rec.Dangling {
		row.Dangling = true
	}
}

// statusRank 越小越该被优先看见，用于排序，也用于「同一引用留哪条记录」的取舍。
func statusRank(st model.Status) int {
	switch st {
	case model.StatusUpdatePending:
		return 0
	case model.StatusNewTag:
		return 1
	case "":
		return 2
	case model.StatusUnknown:
		return 3
	case model.StatusUpToDate:
		return 4
	default:
		return 5
	}
}

// splitImageTag 拆出镜像名与 tag。
func splitImageTag(ref string) (name, tag string) {
	if ref == "" {
		return "", ""
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	// 没写 tag 的引用，Docker 一律当作 latest
	return ref, "latest"
}

// labelImageName 尽力为悬空镜像找回一个可读的名字。
//
// 悬空镜像（RepoTags 为空）在 Docker 里没有任何 tag，`docker images` 也只显示
// <none>，所以「镜像名」确实取不到。但镜像**构建时写进 Config 的标签**会一直
// 留着，其中 org.opencontainers.image.title / source 这类往往带着原始名字或
// 源码仓库地址 —— 这是找回名字最靠谱的一条路。
//
// 按「越像镜像名的越优先」依次尝试，全都没有就返回空串。**绝不自造名字**：
// 编一个看着像模像样的名字出来，比诚实地显示短 ID 更糟。
func labelImageName(img dockerx.Image) string {
	// 直接就是名字的标签
	for _, key := range []string{
		"org.opencontainers.image.title",
		"org.label-schema.name",
		"org.opencontainers.image.ref.name",
	} {
		if v := strings.TrimSpace(img.Labels[key]); v != "" {
			if name, _ := splitImageTag(v); name != "" {
				return name
			}
		}
	}
	// source / vcs-url 一般是源码仓库地址，取最后一段当名字
	for _, key := range []string{
		"org.opencontainers.image.source",
		"org.label-schema.vcs-url",
	} {
		if v := strings.TrimSpace(img.Labels[key]); v != "" {
			if name := lastPathSegment(v); name != "" {
				return name
			}
		}
	}
	// 摘要引用形如 alpine@sha256:…，能取到仓库名
	for _, d := range img.RepoDigests {
		if i := strings.Index(d, "@"); i > 0 {
			if name, _ := splitImageTag(d[:i]); name != "" {
				return name
			}
		}
	}
	return ""
}

// lastPathSegment 取 URL / 路径的最后一段，并去掉 .git 后缀。
func lastPathSegment(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

type addImageReq struct {
	Reference string `json:"reference"`
}

// handleAddImage 手动添加一个纯远端监控项。
func (s *Server) handleAddImage(w http.ResponseWriter, r *http.Request) {
	var in addImageReq
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	ref, err := registry.ParseReference(in.Reference)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "镜像引用无效: "+err.Error())
		return
	}
	img := &model.Image{
		Reference: ref.String(),
		Origin:    model.OriginWatch,
		LocalTag:  ref.Tag,
		Status:    model.StatusUnknown,
	}
	id, err := s.st.UpsertImage(img)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "写入监控项失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"id": id, "reference": img.Reference})
}

func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.DeleteImage(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"deleted": id})
}

type ignoreReq struct {
	Ignored bool `json:"ignored"`
}

func (s *Server) handleIgnoreImage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var in ignoreReq
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if err := s.st.SetImageIgnored(id, in.Ignored); err != nil {
		writeErr(w, http.StatusInternalServerError, "更新失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"id": id, "ignored": in.Ignored})
}

type modeReq struct {
	Mode     string `json:"mode"`     // auto / digest-only / pin-watch
	Override bool   `json:"override"` // 是否固定该模式（false 表示恢复自动判定）
}

func (s *Server) handleModeImage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var in modeReq
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	mode := model.DetectMode(in.Mode)
	switch mode {
	case model.ModeAuto, model.ModeDigestOnly, model.ModePinWatch:
	default:
		writeErr(w, http.StatusBadRequest, "检测模式无效: "+in.Mode)
		return
	}
	if err := s.st.SetImageMode(id, mode, in.Override); err != nil {
		writeErr(w, http.StatusInternalServerError, "更新失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"id": id, "mode": mode, "override": in.Override})
}

type pullImageReq struct {
	Reference string `json:"reference"`
}

// handlePullImage 重新拉取一个镜像，用于「没有容器在用」的镜像。
//
// 拉取可能持续好几分钟，所以建一条任务后立刻返回，进度去「更新历史」看，
// 而不是把 HTTP 连接挂在那里干等。
func (s *Server) handlePullImage(w http.ResponseWriter, r *http.Request) {
	var in pullImageReq
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	ref, err := registry.ParseReference(in.Reference)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "镜像引用无效: "+err.Error())
		return
	}
	key := ref.String()

	if s.up.Busy() {
		writeErr(w, http.StatusConflict, "已有更新任务正在执行，请稍后重试")
		return
	}

	taskID, err := s.st.CreateTask(&model.Task{
		ImageRef:      key,
		ContainerName: key,
		Status:        model.TaskPending,
		TriggeredBy:   "web",
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建任务失败: "+err.Error())
		return
	}

	// 这里不能用 r.Context()：handler 一返回它就撤销了，后台任务会当场夭折。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if err := s.up.Pull(ctx, updater.PullRequest{TaskID: taskID, ImageRef: key}); err != nil {
			s.log.Warn("拉取镜像失败", "ref", key, "err", err)
		}
	}()

	writeOK(w, map[string]any{"task_id": taskID, "reference": key})
}

type removeLocalImagesReq struct {
	IDs   []string `json:"ids"`   // 镜像 ID（DELETE /images/{id} 用的那个）
	Force bool     `json:"force"` // 强制：允许删除仍被容器占用的镜像
}

type removeLocalImageResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// handleRemoveLocalImages 删除本机镜像，一次可以删多个。
//
// 删镜像是不可逆的（层被回收后只能重新拉），所以对「仍被容器占用」的镜像
// 一律拒绝，除非显式 force。特别注意：读不到容器列表时必须整体中止，
// 否则「判断不了谁在用」会退化成「全都删掉」，那是灾难。
func (s *Server) handleRemoveLocalImages(w http.ResponseWriter, r *http.Request) {
	var in removeLocalImagesReq
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if len(in.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "没有要删除的镜像")
		return
	}

	inUse := map[string]bool{}
	containers, cerr := s.dk.ListContainers(r.Context())
	if cerr != nil {
		writeErr(w, http.StatusInternalServerError, "读取容器列表失败，为免误删已中止："+cerr.Error())
		return
	}
	for _, c := range containers {
		if c.ImageID != "" {
			inUse[c.ImageID] = true
		}
	}

	results := make([]removeLocalImageResult, 0, len(in.IDs))
	removed := 0
	for _, id := range in.IDs {
		res := removeLocalImageResult{ID: id}
		switch {
		case id == "":
			res.Error = "镜像 ID 为空"
		case inUse[id] && !in.Force:
			// 这些镜像在容器列表里还能看到，用户应该去那边处理
			res.Error = "仍被容器使用，已跳过"
		default:
			if err := s.dk.RemoveImage(r.Context(), id, in.Force); err != nil {
				res.Error = err.Error()
				break
			}
			// 镜像没了，对应的监控记录也没必要留着
			if derr := s.st.DeleteImageByImageID(id); derr != nil {
				s.log.Warn("删除镜像记录失败", "id", id, "err", derr)
			}
			res.OK = true
			removed++
		}
		results = append(results, res)
	}

	writeOK(w, map[string]any{"results": results, "removed": removed, "total": len(results)})
}

// handleScan 触发一次扫描。?force=1 为强制扫描（忽略去重，重新广播）。
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if s.sc.Busy() {
		writeJSON(w, http.StatusOK, map[string]any{"result": "scan already running"})
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	sc, err := s.sc.Scan(r.Context(), force)
	if err != nil {
		if errors.Is(err, scanner.ErrScanRunning) {
			writeJSON(w, http.StatusOK, map[string]any{"result": "scan already running"})
			return
		}
		writeErr(w, http.StatusInternalServerError, "扫描失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"result": "done", "scan": sc})
}

func (s *Server) handleScans(w http.ResponseWriter, r *http.Request) {
	items, err := s.st.ListScans(20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取扫描历史失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"items": items})
}

func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	unreadOnly := r.URL.Query().Get("unread") == "1"
	items, err := s.st.ListNotifications(unreadOnly, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取通知失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"items": items, "total": len(items)})
}

func (s *Server) handleReadNotification(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.MarkNotificationRead(id, true); err != nil {
		writeErr(w, http.StatusInternalServerError, "标记失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"id": id})
}

func (s *Server) handleReadAll(w http.ResponseWriter, r *http.Request) {
	if err := s.st.MarkAllRead(); err != nil {
		writeErr(w, http.StatusInternalServerError, "标记失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"result": "ok"})
}

func (s *Server) handleClearRead(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.DeleteReadNotifications()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "清理失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"deleted": n})
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	items, err := s.st.ListTasks(100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取更新历史失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"items": items})
}

func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := s.st.GetTask(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "任务不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "读取任务失败: "+err.Error())
		return
	}
	writeOK(w, t)
}

type manualUpdateReq struct {
	NotificationID int64 `json:"notification_id"`
	ImageID        int64 `json:"image_id"`
}

// handleManualUpdate 从页面手动触发更新（与飞书按钮等价）。
func (s *Server) handleManualUpdate(w http.ResponseWriter, r *http.Request) {
	var in manualUpdateReq
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}

	var n *model.Notification
	switch {
	case in.NotificationID > 0:
		got, err := s.st.GetNotification(in.NotificationID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "通知记录不存在")
			return
		}
		n = got
	case in.ImageID > 0:
		img, err := s.st.GetImage(in.ImageID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "监控项不存在")
			return
		}
		// 从监控项直接发起更新（页面上「立即更新」按钮）。
		// LatestTag 非空即代表这是「发现更高版本 tag」的场景，
		// 据此标记 Kind，后续取目标引用与实际拉取的镜像都靠它。
		kind := model.NotifNewVersion
		if img.LatestTag != "" && img.LatestTag != img.LocalTag {
			kind = model.NotifNewTag
		}
		n = &model.Notification{
			ImageRef:      img.Reference,
			ContainerID:   img.ContainerID,
			ContainerName: img.ContainerName,
			Kind:          kind,
			LocalTag:      img.LocalTag,
			LatestTag:     img.LatestTag,
			LocalSummary:  "",
			RemoteSummary: "",
		}
	default:
		writeErr(w, http.StatusBadRequest, "需要提供 notification_id 或 image_id")
		return
	}

	taskID, err := s.nt.TriggerUpdate(n, "web")
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeOK(w, map[string]any{"task_id": taskID})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeOK(w, s.LoadSettings())
}

// handlePutSettings 保存运行时设置并即时生效。
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in model.Settings
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if in.ScanInterval < 0 {
		writeErr(w, http.StatusBadRequest, "扫描间隔不能为负")
		return
	}
	if in.ScanInterval > 0 && in.ScanInterval < 30 {
		writeErr(w, http.StatusBadRequest, "扫描间隔最小为 30 秒")
		return
	}
	if in.HealthCheckWait < 0 || in.HealthCheckWait > 600 {
		writeErr(w, http.StatusBadRequest, "健康检查等待时间需在 0-600 秒之间")
		return
	}
	if err := s.saveSettings(in); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存设置失败: "+err.Error())
		return
	}
	s.ApplySettings(in)
	writeOK(w, s.LoadSettings())
}

func (s *Server) handleFeishuTest(w http.ResponseWriter, r *http.Request) {
	if err := s.nt.SendTestCard(r.Context()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, map[string]any{"result": "sent"})
}

// handleFeishuStatus 汇报飞书链路状态。
//
// 「卡片发得出去」与「按钮点得动」是两条独立链路，只发一张卡片证明不了后者。
// 这里把「最近一次按钮回调的时间」暴露给设置页，让验证从「去群里看看」变成
// 一个有确定结论的等待过程。
func (s *Server) handleFeishuStatus(w http.ResponseWriter, r *http.Request) {
	at, action, count := s.nt.CallbackState()
	resp := map[string]any{
		"configured":     s.nt.Feishu().Configured(),
		"long_conn":      s.nt.LongConnRunning(),
		"callback_count": count,
		"last_action":    action,
	}
	if !at.IsZero() {
		resp["last_callback_at"] = at.Format(time.RFC3339)
	}
	writeOK(w, resp)
}

type registryTestReq struct {
	Reference string `json:"reference"`
}

// handleRegistryTest 逐个探测注册表与各加速源的连通情况。
//
// 镜像列表里全是「未知」时，用户只看到三个字，无从下手。
// 这个接口把「直连通不通、加速站通不通、tag 到底在不在」逐条摆出来，
// 让排查从「猜」变成「看」。
func (s *Server) handleRegistryTest(w http.ResponseWriter, r *http.Request) {
	var in registryTestReq
	if r.ContentLength > 0 {
		if err := decodeBody(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
			return
		}
	}

	// 不填就用一个极小的官方镜像当探针，它几乎不会从任何加速站的下架名单里消失
	raw := strings.TrimSpace(in.Reference)
	if raw == "" {
		raw = "hello-world:latest"
	}
	ref, err := registry.ParseReference(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "镜像引用无效: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	writeOK(w, map[string]any{
		"reference":      ref.String(),
		"registry":       ref.Registry,
		"docker_hub":     registry.IsDockerHub(ref.Registry),
		"mirrors":        s.reg.Mirrors(),
		"results":        s.reg.Probe(ctx, ref),
		"daemon_mirrors": s.daemonRegistryMirrors(ctx),
	})
}

// daemonRegistryMirrors 读取 Docker 守护进程自己配置的加速器。
//
// 飞牛上的 Docker 能正常拉镜像，往往是因为 daemon.json 里早就配了
// registry-mirrors；而本服务查 digest 走的是自己发起的 HTTP 请求，
// 不会自动继承那份配置，于是出现「镜像拉得动、检测全未知」的怪象。
// 把这份配置读出来，既能在诊断里显示，也能在设置页一键填进去。
func (s *Server) daemonRegistryMirrors(ctx context.Context) []string {
	if s.dk == nil {
		return nil
	}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	mirrors, err := s.dk.RegistryMirrors(tctx)
	if err != nil {
		return nil
	}
	return registry.ParseMirrors(strings.Join(mirrors, ","))
}

// ---- 设置读写 ----

const settingsKey = "runtime"

// LoadSettings 读取运行时设置，缺省回落到环境变量提供的初值。
func (s *Server) LoadSettings() model.Settings {
	authUser, authPass := s.auth()
	out := model.Settings{
		ScanInterval:      int(s.cfg.ScanInterval / time.Second),
		AutoUpdate:        s.cfg.AutoUpdateEnabled,
		RequireConfirm:    s.cfg.RequireConfirm,
		RollbackOnFailure: s.cfg.RollbackOnFailure,
		KeepOldContainer:  s.cfg.KeepOldContainer,
		HealthCheckWait:   int(s.cfg.HealthCheckWait / time.Second),
		RegistryMirror:    s.cfg.RegistryMirror,
		RegistryInsecure:  s.cfg.RegistryInsecure,
		FeishuAppID:       s.cfg.FeishuAppID,
		FeishuAppSecret:   s.cfg.FeishuAppSecret,
		FeishuChatID:      s.cfg.FeishuChatID,
		AdminUser:         authUser,
		AdminPassword:     authPass,
	}
	raw, ok, err := s.st.GetSetting(settingsKey)
	if err == nil && ok && raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (s *Server) saveSettings(in model.Settings) error {
	buf, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return s.st.SetSetting(settingsKey, string(buf))
}

// ApplySettings 把设置推送到各组件，无需重启即可生效。
func (s *Server) ApplySettings(in model.Settings) {
	s.sc.SetOptions(scanner.Options{
		ExtraWatch:        s.cfg.Watch,
		ExcludeContainers: nil,
		SelfContainer:     s.cfg.SelfContainer,
		SelfContainerID:   s.cfg.SelfContainerID,
		Concurrency:       4,
	})
	s.up.SetOptions(updater.Options{
		PullTimeout:       s.cfg.PullTimeout,
		StopTimeoutSec:    30,
		HealthCheckWait:   time.Duration(in.HealthCheckWait) * time.Second,
		RollbackOnFailure: in.RollbackOnFailure,
		KeepOldContainer:  in.KeepOldContainer,
		SelfContainer:     s.cfg.SelfContainer,
		SelfContainerID:   s.cfg.SelfContainerID,
	})
	// 注册表加速也必须在这里下发：早先只把它交给启动时的环境变量，
	// 于是用户在设置页填的加速地址保存后毫无作用，检测照样全是「未知」。
	if s.reg != nil {
		mirrors := in.RegistryMirror
		if strings.TrimSpace(mirrors) == "" {
			// 设置页留空时沿用 Docker 守护进程里配好的加速器：
			// 那台机器既然拉得动镜像，多半就是靠它，没道理让本服务反着来。
			mirrors = s.cfg.RegistryMirror
		}
		s.reg.SetOptions(registry.Options{
			Mirrors:  registry.ParseMirrors(mirrors),
			Insecure: in.RegistryInsecure,
			Auth:     s.cfg.RegistryAuth,
		})
	}
	// 访问密码可以在页面里改：留空即关闭登录，保存后下一个请求就按新凭据校验。
	s.setAuth(in.AdminUser, in.AdminPassword)
	s.nt.ApplyFeishu(in.FeishuAppID, in.FeishuAppSecret, in.FeishuChatID)
}
