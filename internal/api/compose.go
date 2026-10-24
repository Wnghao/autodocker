package api

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/updater"
)

// compose 配置是几百行的小文件，超过这个大小肯定不是配置（多半是路径写错了）。
const composeFileLimit = 1 << 20

type composeFileView struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Exists  bool   `json:"exists"`
	Error   string `json:"error,omitempty"`
}

type composeView struct {
	Project   string            `json:"project"`
	WorkDir   string            `json:"workdir"`
	Mounted   bool              `json:"mounted"`
	HostRoot  string            `json:"host_root"`
	MountRoot string            `json:"mount_root"`
	Files     []composeFileView `json:"files"`
}

// handleComposeView 返回项目里那几份 compose 文件的内容。
//
// 文件路径只认容器标签里列出的 config_files：页面能读的永远只有
// 「更新时真正会用到的那几份」，不是一个通用文件浏览器。
func (s *Server) handleComposeView(w http.ResponseWriter, r *http.Request) {
	containerID := strings.TrimSpace(r.URL.Query().Get("container"))
	if containerID == "" {
		writeErr(w, http.StatusBadRequest, "缺少 container 参数")
		return
	}
	p, err := s.up.ComposeProjectOf(r.Context(), containerID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}

	out := composeView{
		Project:   p.Project,
		WorkDir:   p.WorkDir,
		HostRoot:  s.cfg.ComposeHostRoot,
		MountRoot: s.cfg.ComposeMountRoot,
	}
	if fi, serr := os.Stat(s.cfg.ComposeMountRoot); serr == nil && fi.IsDir() {
		out.Mounted = true
	}

	for _, f := range p.Files {
		v := composeFileView{Path: f}
		if !out.Mounted {
			v.Error = fmt.Sprintf("面板没有挂载宿主机目录，读不到文件；请给面板容器加 - %s:%s 后重建",
				s.cfg.ComposeHostRoot, s.cfg.ComposeMountRoot)
			out.Files = append(out.Files, v)
			continue
		}
		local, merr := mapHostPath(s.cfg.ComposeHostRoot, s.cfg.ComposeMountRoot, f)
		if merr != nil {
			v.Error = merr.Error()
			out.Files = append(out.Files, v)
			continue
		}
		data, rerr := readComposeFile(local)
		switch {
		case errors.Is(rerr, fs.ErrNotExist):
			v.Error = "文件不存在（compose 标签里的路径可能已经过时，重新部署一次项目即可刷新）"
		case rerr != nil:
			v.Error = rerr.Error()
		default:
			v.Content = string(data)
			v.Exists = true
		}
		out.Files = append(out.Files, v)
	}
	writeOK(w, out)
}

type composeSaveReq struct {
	Container string `json:"container"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

// handleComposeSave 覆盖一份 compose 文件，写盘前先留一份同目录备份。
func (s *Server) handleComposeSave(w http.ResponseWriter, r *http.Request) {
	var in composeSaveReq
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Container) == "" {
		writeErr(w, http.StatusBadRequest, "缺少 container 参数")
		return
	}
	if len(in.Content) > composeFileLimit {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("内容超过 %d KB，不像是 compose 配置", composeFileLimit/1024))
		return
	}

	p, err := s.up.ComposeProjectOf(r.Context(), in.Container)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if !projectHasFile(p.Files, in.Path) {
		writeErr(w, http.StatusBadRequest, "只能修改这个项目自己的 compose 文件")
		return
	}
	if fi, serr := os.Stat(s.cfg.ComposeMountRoot); serr != nil || !fi.IsDir() {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("面板没有挂载宿主机目录，写不进去；请给面板容器加 - %s:%s 后重建",
			s.cfg.ComposeHostRoot, s.cfg.ComposeMountRoot))
		return
	}
	local, err := mapHostPath(s.cfg.ComposeHostRoot, s.cfg.ComposeMountRoot, in.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if fi, serr := os.Stat(local); serr != nil || !fi.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, "目标文件不存在，不新建文件")
		return
	}

	backup, changed, err := writeComposeFile(local, in.Content)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{
		"path":      in.Path,
		"backup":    unmapLocalPath(s.cfg.ComposeHostRoot, s.cfg.ComposeMountRoot, backup),
		"unchanged": !changed,
	})
}

type composeApplyReq struct {
	Container string `json:"container"`
}

// handleComposeApply 对整份项目执行 compose up -d，把页面上的改动变成实际状态。
func (s *Server) handleComposeApply(w http.ResponseWriter, r *http.Request) {
	var in composeApplyReq
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Container) == "" {
		writeErr(w, http.StatusBadRequest, "缺少 container 参数")
		return
	}
	p, err := s.up.ComposeProjectOf(r.Context(), in.Container)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	res, err := s.up.ComposeApply(r.Context(), p)
	if err != nil {
		if errors.Is(err, updater.ErrBusy) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"exit": res.Exit, "output": res.Output})
}

func projectHasFile(files []string, want string) bool {
	want = path.Clean(want)
	for _, f := range files {
		if path.Clean(f) == want {
			return true
		}
	}
	return false
}

// mapHostPath 把宿主机的绝对路径映射到面板容器里能看到的位置。
// 只接受挂在 hostRoot 之下的路径，别的一律不读 —— 这里不是文件浏览器。
func mapHostPath(hostRoot, mountRoot, p string) (string, error) {
	root := path.Clean(hostRoot)
	clean := path.Clean(p)
	if root == "." || root == "/" {
		return "", errors.New("COMPOSE_HOST_ROOT 配置无效")
	}
	if !strings.HasPrefix(clean, root+"/") {
		return "", fmt.Errorf("文件不在 %s 下，面板不读取", root)
	}
	rel := strings.TrimPrefix(clean, root+"/")
	if !fs.ValidPath(rel) {
		return "", errors.New("非法的文件路径")
	}
	return path.Join(path.Clean(mountRoot), rel), nil
}

// unmapLocalPath 把挂载点里的路径换回宿主机上的写法，用于把备份路径回给页面。
// 页面上的路径一律是宿主机路径，否则用户在 1Panel 里按这个路径找不到文件。
func unmapLocalPath(hostRoot, mountRoot, local string) string {
	if local == "" {
		return ""
	}
	mount := path.Clean(mountRoot)
	clean := path.Clean(local)
	if !strings.HasPrefix(clean, mount+"/") {
		return local
	}
	return path.Join(path.Clean(hostRoot), strings.TrimPrefix(clean, mount+"/"))
}

func readComposeFile(local string) ([]byte, error) {
	fi, err := os.Stat(local)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("不是普通文件")
	}
	if fi.Size() > composeFileLimit {
		return nil, fmt.Errorf("文件超过 %d KB，不像是 compose 配置", composeFileLimit/1024)
	}
	return os.ReadFile(local)
}

// writeComposeFile 先备份再原子替换：写一半失败时项目里仍是原来那份文件。
func writeComposeFile(local, content string) (backup string, changed bool, err error) {
	fi, err := os.Stat(local)
	if err != nil {
		return "", false, fmt.Errorf("读取原文件失败: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", false, errors.New("目标不是普通文件")
	}
	old, err := os.ReadFile(local)
	if err != nil {
		return "", false, fmt.Errorf("读取原文件失败: %w", err)
	}
	if string(old) == content {
		return "", false, nil
	}

	backup = availableBackupPath(local)
	if werr := os.WriteFile(backup, old, fi.Mode().Perm()); werr != nil {
		return "", false, fmt.Errorf("写备份失败: %w", werr)
	}
	tmp, err := os.CreateTemp(filepath.Dir(local), ".autodocker-*.tmp")
	if err != nil {
		return backup, false, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里是空操作
	if _, err = tmp.WriteString(content); err == nil {
		err = tmp.Chmod(fi.Mode().Perm())
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return backup, false, fmt.Errorf("写临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, local); err != nil {
		return backup, false, fmt.Errorf("替换原文件失败: %w", err)
	}
	return backup, true, nil
}

func availableBackupPath(local string) string {
	base := local + ".bak-" + time.Now().Format("20060102-150405")
	out := base
	for i := 2; ; i++ {
		if _, err := os.Stat(out); errors.Is(err, fs.ErrNotExist) {
			return out
		}
		out = fmt.Sprintf("%s-%d", base, i)
	}
}
