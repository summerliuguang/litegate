// Package web 内嵌管理页面，随二进制一起发布。
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html
var content embed.FS

// Handler 返回内嵌页面的文件服务。
func Handler() http.Handler {
	sub, err := fs.Sub(content, ".")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
