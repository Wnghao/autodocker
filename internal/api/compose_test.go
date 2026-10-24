package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMapHostPath(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		mount   string
		in      string
		want    string
		wantErr bool
	}{
		{name: "普通项目文件", host: "/opt/1panel", mount: "/host/1panel",
			in:   "/opt/1panel/docker/compose/flatnas/docker-compose.yml",
			want: "/host/1panel/docker/compose/flatnas/docker-compose.yml"},
		{name: "根目录下的文件", host: "/opt/1panel", mount: "/host/1panel",
			in: "/opt/1panel/a.yml", want: "/host/1panel/a.yml"},
		{name: "根目录之外", host: "/opt/1panel", mount: "/host/1panel",
			in: "/etc/passwd", wantErr: true},
		{name: "前缀相似但不在根下", host: "/opt/1panel", mount: "/host/1panel",
			in: "/opt/1panel2/a.yml", wantErr: true},
		{name: "路径穿越", host: "/opt/1panel", mount: "/host/1panel",
			in: "/opt/1panel/../etc/passwd", wantErr: true},
		{name: "相对路径", host: "/opt/1panel", mount: "/host/1panel",
			in: "docker-compose.yml", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mapHostPath(tc.host, tc.mount, tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应拒绝 %q，实际映射到 %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("映射失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("映射结果 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestUnmapLocalPath(t *testing.T) {
	got := unmapLocalPath("/opt/1panel", "/host/1panel",
		"/host/1panel/docker/compose/nas-apps/docker-compose.yml.bak-20261024-120000")
	want := "/opt/1panel/docker/compose/nas-apps/docker-compose.yml.bak-20261024-120000"
	if got != want {
		t.Fatalf("回写路径 = %q，期望 %q", got, want)
	}
	if got := unmapLocalPath("/opt/1panel", "/host/1panel", ""); got != "" {
		t.Fatalf("空路径应原样返回空，实际 %q", got)
	}
}

func TestProjectHasFile(t *testing.T) {
	files := []string{"/opt/1panel/apps/a/docker-compose.yml", "/opt/1panel/apps/a/override.yml"}
	if !projectHasFile(files, "/opt/1panel/apps/a/docker-compose.yml") {
		t.Fatal("列出的文件应被接受")
	}
	if !projectHasFile(files, "/opt/1panel/apps/a/../a/docker-compose.yml") {
		t.Fatal("Clean 之后相同的路径应被接受")
	}
	if projectHasFile(files, "/opt/1panel/apps/b/docker-compose.yml") {
		t.Fatal("其他项目的文件不应被接受")
	}
}

func TestWriteComposeFileBacksUpAndReplaces(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	old := "services:\n  app:\n    image: app:1\n"
	if err := os.WriteFile(file, []byte(old), 0o640); err != nil {
		t.Fatal(err)
	}
	// 记下实际权限：Windows 上 chmod 不生效，写死 0640 会让这条断言变成平台相关
	origInfo, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	origMode := origInfo.Mode().Perm()

	backup, changed, err := writeComposeFile(file, "services:\n  app:\n    image: app:2\n")
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !changed {
		t.Fatal("内容变了应报告 changed=true")
	}
	if !strings.HasPrefix(backup, file+".bak-") {
		t.Fatalf("备份路径不对: %q", backup)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "app:2") {
		t.Fatalf("新内容没写进去: %q", got)
	}
	gotBak, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBak) != old {
		t.Fatalf("备份内容不是原文件: %q", gotBak)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != origMode {
		t.Fatalf("原文件权限应保留，实际 %v / %v", fi.Mode().Perm(), err)
	}

	// 同样的内容再存一次：不写盘、不再留备份
	backup2, changed2, err := writeComposeFile(file, string(got))
	if err != nil {
		t.Fatal(err)
	}
	if changed2 || backup2 != "" {
		t.Fatalf("内容没变时不应写盘: changed=%v backup=%q", changed2, backup2)
	}
}

func TestWriteComposeFileRefusesMissing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "nope.yml")
	if _, _, err := writeComposeFile(file, "x"); err == nil {
		t.Fatal("目标不存在时应报错，不能顺手新建")
	}
}
