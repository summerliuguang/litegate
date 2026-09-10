// Package web 内嵌管理页面，随二进制一起发布。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed *.html static
var content embed.FS

// Handler 返回内嵌页面的文件服务。
// 页面与静态资源都随二进制发布、每次构建内容可能变化，统一带 no-cache：
// 浏览器每次协商重验（命中则 304），避免手机端缓存旧版 skin.css/common.js
// 导致"改了样式但设备上没变化"。
func Handler() http.Handler {
	sub, err := fs.Sub(content, ".")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
