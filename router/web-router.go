package router

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-contrib/gzip"
	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
)

// ThemeAssets holds the embedded frontend assets for both themes
// plus the standalone Landing v1.0 marketing page.
type ThemeAssets struct {
	DefaultBuildFS   embed.FS
	DefaultIndexPage []byte
	ClassicBuildFS   embed.FS
	ClassicIndexPage []byte

	// Landing v1.0 — 独立营销落地页 + 静态子资源（prompts.html / logo.jpg / favicon.ico）
	// 路由：GET /landing                 → 渲染 LandingIndexPage（带 5 分钟 CDN 缓存）
	//      GET /landing/<resource>      → http.FileServer 直出 web/landing/ 下的任意文件
	LandingBuildFS   embed.FS
	LandingIndexPage []byte
}

func SetWebRouter(router *gin.Engine, assets ThemeAssets) {
	defaultFS := common.EmbedFolder(assets.DefaultBuildFS, "web/default/dist")
	classicFS := common.EmbedFolder(assets.ClassicBuildFS, "web/classic/dist")
	themeFS := common.NewThemeAwareFS(defaultFS, classicFS)

	router.Use(gzip.Gzip(gzip.DefaultCompression))
	router.Use(middleware.GlobalWebRateLimit())
	router.Use(middleware.Cache())

	// ─── Landing v1.0 sub-resources ──────────────────────────────────
	// 之前用 gin-contrib/static.Serve 子路径 404 (logo.jpg/prompts.html 等都被 React NoRoute 截胡)
	// 改成 Go 标准库 http.FileServer + StripPrefix，绝对工作
	if landingSubFS, err := fs.Sub(assets.LandingBuildFS, "web/landing"); err == nil {
		landingFileServer := http.StripPrefix("/landing", http.FileServer(http.FS(landingSubFS)))
		// 子资源路由（/landing/* 必须有路径，否则不匹配，bare /landing 走 NoRoute）
		landingSubHandler := func(c *gin.Context) {
			landingFileServer.ServeHTTP(c.Writer, c.Request)
		}
		router.GET("/landing/*filepath", landingSubHandler)
		router.HEAD("/landing/*filepath", landingSubHandler)
	}

	router.Use(static.Serve("/", themeFS))
	router.NoRoute(func(c *gin.Context) {
		c.Set(middleware.RouteTagKey, "web")
		uri := c.Request.RequestURI
		if strings.HasPrefix(uri, "/v1") || strings.HasPrefix(uri, "/api") || strings.HasPrefix(uri, "/assets") {
			controller.RelayNotFound(c)
			return
		}
		// /landing 与 /landing/ 落到 NoRoute 时，渲染营销页而非 React 主前端
		// 子路径（/landing/prompts.html 等）已在上方 router.GET 处理
		if uri == "/landing" || uri == "/landing/" || strings.HasPrefix(uri, "/landing?") {
			c.Header("Cache-Control", "public, max-age=300")
			c.Data(http.StatusOK, "text/html; charset=utf-8", assets.LandingIndexPage)
			return
		}
		c.Header("Cache-Control", "no-cache")
		if common.GetTheme() == "classic" {
			c.Data(http.StatusOK, "text/html; charset=utf-8", assets.ClassicIndexPage)
		} else {
			c.Data(http.StatusOK, "text/html; charset=utf-8", assets.DefaultIndexPage)
		}
	})
}
