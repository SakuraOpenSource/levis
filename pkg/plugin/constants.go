package plugin

// 环境变量名。插件启动时从这些变量里取得自己需要的一切。
const (
	// EnvToken 是本次会话的调用令牌，插件必须校验每个请求都带着它。
	EnvToken = "LEVIS_PLUGIN_TOKEN"
	// EnvDataDir 是插件的私有可写目录。
	EnvDataDir = "LEVIS_PLUGIN_DATA"
	// EnvAPIBase 是回调主程序的基址，如 http://127.0.0.1:8080/api/plugin/v1。
	EnvAPIBase = "LEVIS_API_BASE"
	// EnvAPIKey 是回调主程序用的 Key，明文只经环境变量传递，不落库。
	EnvAPIKey = "LEVIS_API_KEY"
	// EnvPluginID 让插件知道自己的 ID，便于打日志。
	EnvPluginID = "LEVIS_PLUGIN_ID"
)

// MetadataToken 是 gRPC metadata 中携带令牌的键名。
//
// metadata 的键必须是小写，gRPC 会做规范化，这里直接写成小写避免混淆。
const MetadataToken = "levis-plugin-token"
