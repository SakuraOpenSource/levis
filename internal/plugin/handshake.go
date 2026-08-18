package plugin

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

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

// handshakeTimeout 是等待插件打印握手行的上限。
//
// 插件要做的只是监听端口并打印一行，正常在毫秒级。给到 10 秒是为容忍慢磁盘
// 上的冷启动；再长就该让管理员看到失败，而不是让主程序一直等。
const handshakeTimeout = 10 * time.Second

// callTimeout 是生命周期类 RPC 的超时。
const callTimeout = 5 * time.Second

// handshake 是插件启动后向 stdout 打印的第一行 JSON。
type handshake struct {
	// Port 是插件 gRPC 服务监听的端口，必须在 127.0.0.1 上。
	Port int `json:"port"`
}

// generateToken 生成一个 32 字节的随机令牌。
func generateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成插件令牌失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// conn 是与某个插件进程的连接。
type conn struct {
	cmd    *exec.Cmd
	client pb.PluginClient
	grpc   *grpc.ClientConn
	token  string
}

// dialOptions 是与插件建立连接时的选项。
//
// 明文传输：连接只在 127.0.0.1 上，且已有令牌做调用方认证；为环回地址签发
// 证书需要一套本地 CA，收益不足以抵掉复杂度。
func dialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
}

// launch 启动插件进程并完成握手，返回可用的连接。
//
// 失败时保证进程已被清理，不会留下孤儿。
func launch(ctx context.Context, spec Found, env []string, logf func(string, ...any)) (*conn, error) {
	token, err := generateToken()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(spec.Path)
	// 工作目录设为插件自己的目录，插件用相对路径读自带资源时才符合直觉。
	cmd.Dir = spec.DataDir
	// 只给插件明确需要的变量，不继承主程序的整个环境 —— 那里面可能有
	// 数据库密码之类的东西，没有理由交给插件。
	cmd.Env = append([]string{
		EnvToken + "=" + token,
		EnvPluginID + "=" + spec.ID,
		EnvDataDir + "=" + spec.DataDir,
	}, env...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("创建插件输出管道失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("创建插件错误管道失败: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动插件失败: %w", err)
	}

	// 出错时确保进程被收掉。成功路径上把 cleanup 置为 nil。
	cleanup := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	// stderr 全程转发到主程序日志，插件的报错才有地方可看。
	go forward(stderr, spec.ID, logf)

	reader := bufio.NewReader(stdout)
	hs, err := readHandshake(ctx, reader)
	if err != nil {
		return nil, err
	}
	// 握手行之后的 stdout 同样转发，插件不必区分该往哪个流写日志。
	go forward(reader, spec.ID, logf)

	target := fmt.Sprintf("127.0.0.1:%d", hs.Port)
	grpcConn, err := grpc.NewClient(target, dialOptions()...)
	if err != nil {
		return nil, fmt.Errorf("连接插件失败: %w", err)
	}

	c := &conn{cmd: cmd, client: pb.NewPluginClient(grpcConn), grpc: grpcConn, token: token}
	cleanup = nil
	return c, nil
}

// readHandshake 读取插件打印的握手行。
//
// 单独起 goroutine 读、用 select 等：ReadString 本身不接受 context，插件若
// 挂住不输出，直接读会永久阻塞。
func readHandshake(ctx context.Context, reader *bufio.Reader) (*handshake, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := reader.ReadString('\n')
		ch <- result{line: line, err: err}
	}()

	timer := time.NewTimer(handshakeTimeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("等待插件握手超时（%s 内未输出握手信息）", handshakeTimeout)
	case res := <-ch:
		if res.err != nil && strings.TrimSpace(res.line) == "" {
			// 进程启动即退出时最常走到这里，把它说清楚而不是只报 EOF。
			return nil, fmt.Errorf("插件未输出握手信息即退出: %w", res.err)
		}
		var hs handshake
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.line)), &hs); err != nil {
			return nil, fmt.Errorf("解析插件握手信息失败（应为一行 JSON，实际为 %q）", trim(res.line))
		}
		if hs.Port <= 0 || hs.Port > 65535 {
			return nil, fmt.Errorf("插件返回的端口无效: %d", hs.Port)
		}
		return &hs, nil
	}
}

// forward 把插件的输出逐行转发到日志，带插件 ID 前缀。
func forward(r io.Reader, id string, logf func(string, ...any)) {
	scanner := bufio.NewScanner(r)
	// 插件可能打印很长的行（比如渠道返回的报文），放宽到 1 MiB 再截断。
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		logf("[plugin:%s] %s", id, scanner.Text())
	}
}

// withToken 把令牌塞进 outgoing metadata。
//
// 每个调用都带，包括健康检查：插件那边是无状态校验，没有「已认证的连接」
// 这个概念 —— 连接可以被同机的其它进程建立。
func (c *conn) withToken(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, MetadataToken, c.token)
}

// call 是带超时与令牌的调用包装。
func (c *conn) call(ctx context.Context, timeout time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(c.withToken(ctx), timeout)
	defer cancel()
	return fn(ctx)
}

// describe 取插件的自我描述。
func (c *conn) describe(ctx context.Context) (*pb.Manifest, error) {
	var out *pb.Manifest
	err := c.call(ctx, callTimeout, func(ctx context.Context) error {
		reply, err := c.client.Describe(ctx, &pb.DescribeRequest{})
		if err != nil {
			return err
		}
		out = reply
		return nil
	})
	return out, err
}

// configure 下发配置。插件返回的 error 字段表示配置不可用。
func (c *conn) configure(ctx context.Context, values map[string]string) error {
	return c.call(ctx, callTimeout, func(ctx context.Context) error {
		reply, err := c.client.Configure(ctx, &pb.ConfigureRequest{Values: values})
		if err != nil {
			return err
		}
		if reply.GetError() != "" {
			return fmt.Errorf("插件拒绝了当前配置: %s", reply.GetError())
		}
		return nil
	})
}

// health 探测存活。
func (c *conn) health(ctx context.Context) error {
	return c.call(ctx, callTimeout, func(ctx context.Context) error {
		reply, err := c.client.Health(ctx, &pb.HealthRequest{})
		if err != nil {
			return err
		}
		if !reply.GetOk() {
			return fmt.Errorf("插件报告异常: %s", reply.GetMessage())
		}
		return nil
	})
}

// close 关闭连接并结束进程。
//
// 三级递进：先请插件自己收尾，再 SIGTERM，最后 SIGKILL。每一级都给出时限，
// 保证 close 一定会返回 —— 主程序关闭时卡在这里比杀掉插件更糟。
func (c *conn) close(shutdownWait, termWait time.Duration) {
	if c.client != nil {
		ctx, cancel := context.WithTimeout(c.withToken(context.Background()), shutdownWait)
		_, _ = c.client.Shutdown(ctx, &pb.ShutdownRequest{})
		cancel()
	}
	if c.grpc != nil {
		_ = c.grpc.Close()
	}
	if c.cmd == nil || c.cmd.Process == nil {
		return
	}

	done := make(chan struct{})
	go func() {
		_ = c.cmd.Wait()
		close(done)
	}()

	// Shutdown 返回后进程通常已在退出途中，先等一小会儿。
	select {
	case <-done:
		return
	case <-time.After(shutdownWait):
	}

	_ = c.cmd.Process.Signal(sigTerm())
	select {
	case <-done:
		return
	case <-time.After(termWait):
	}

	_ = c.cmd.Process.Kill()
	<-done
}

// trim 截断过长的字符串，用于错误信息。
func trim(s string) string {
	s = strings.TrimSpace(s)
	const limit = 120
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// defaultLogf 是未指定日志函数时的兜底。
func defaultLogf(format string, args ...any) {
	log.Printf(format, args...)
}
