// version 包提供网关的构建版本信息。
//
// 发布构建通过以下方式覆盖 Version：
//
//	go build -ldflags "-X github.com/oyyyyy61/model-gateway/internal/version.Version=vX.Y.Z"
package version

// Version 是可读的网关版本号，会出现在 /metrics 和 /admin/status 中。
var Version = "0.2.0"
