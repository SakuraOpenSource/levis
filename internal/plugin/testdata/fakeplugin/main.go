// Command fakeplugin 是测试用的插件，可通过环境变量切换行为。
//
// 它同时充当契约的最小实现示例：一个真插件要做的就是这些 —— 校验令牌、
// 监听环回地址、打印一行握手 JSON、实现自己声明的能力。
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/SakuraOpenSource/levis/internal/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// 测试通过这些环境变量指定异常行为。
const (
	envMode = "FAKE_MODE"
	// envMailFail 让 SendMail 固定失败，用于验证发信失败不影响业务。
	envMailFail = "FAKE_MAIL_FAIL"
	// envHookDelay 让能力调用先睡一段时间，用于验证超时。
	envHookDelay = "FAKE_HOOK_DELAY"
	// envCallLog 指定一个文件，每次能力调用追加一行，供测试断言调用时机。
	envCallLog = "FAKE_CALL_LOG"
)

// 行为模式。
const (
	// modeNormal 是正常插件。
	modeNormal = ""
	// modeSilent 启动后不打印握手行，用于验证握手超时。
	modeSilent = "silent"
	// modeExit 启动后立刻退出，用于验证退避重启。
	modeExit = "exit"
	// modeBadHandshake 打印一行非 JSON。
	modeBadHandshake = "bad-handshake"
	// modeUnhealthy 让 Health 固定返回 not ok。
	modeUnhealthy = "unhealthy"
)

func main() {
	switch os.Getenv(envMode) {
	case modeSilent:
		// 睡够长，让主程序的握手超时先触发。
		time.Sleep(2 * time.Minute)
		return
	case modeExit:
		fmt.Fprintln(os.Stderr, "假插件按要求立刻退出")
		os.Exit(1)
	case modeBadHandshake:
		fmt.Println("这不是 JSON")
		time.Sleep(2 * time.Minute)
		return
	}

	token := os.Getenv(plugin.EnvToken)
	if token == "" {
		fmt.Fprintln(os.Stderr, "缺少令牌")
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "监听失败:", err)
		os.Exit(1)
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(authInterceptor(token)))
	srv := &fake{server: server}
	pb.RegisterPluginServer(server, srv)

	// 握手行必须是 stdout 的第一行，且之后不再往 stdout 写别的东西。
	port := listener.Addr().(*net.TCPAddr).Port
	line, _ := json.Marshal(map[string]int{"port": port})
	fmt.Println(string(line))

	if err := server.Serve(listener); err != nil {
		fmt.Fprintln(os.Stderr, "服务退出:", err)
	}
}

// authInterceptor 校验每个请求携带的令牌。
//
// 监听环回地址只挡住了外网，同机的其它进程照样能连上来，所以逐调用校验。
// 用 ConstantTimeCompare 而不是 == ：比较凭证时不该泄露时序信息。
func authInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context, req any,
		info *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
	) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "缺少令牌")
		}
		values := md.Get(plugin.MetadataToken)
		if len(values) == 0 {
			return nil, status.Error(codes.Unauthenticated, "缺少令牌")
		}
		if subtle.ConstantTimeCompare([]byte(values[0]), []byte(token)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "令牌不匹配")
		}
		return handler(ctx, req)
	}
}

type fake struct {
	pb.UnimplementedPluginServer
	server *grpc.Server
}

func (f *fake) Describe(context.Context, *pb.DescribeRequest) (*pb.Manifest, error) {
	return &pb.Manifest{
		Name:        "假插件",
		Version:     "0.0.1",
		Description: "仅用于测试",
		Capabilities: []pb.Capability{
			pb.Capability_CAPABILITY_SEND_MAIL,
			pb.Capability_CAPABILITY_CREATE_PAYMENT,
		},
		Config: []*pb.ConfigField{
			{Key: "host", Label: "服务器", Type: pb.FieldType_FIELD_TYPE_TEXT, Required: true},
			{Key: "password", Label: "密码", Type: pb.FieldType_FIELD_TYPE_TEXT, Secret: true},
		},
		RequiredScopes: []string{"wallet:credit", "user:read"},
	}, nil
}

func (f *fake) Configure(_ context.Context, req *pb.ConfigureRequest) (*pb.ConfigureReply, error) {
	// 便于测试断言配置确实下发到了插件。
	fmt.Fprintf(os.Stderr, "收到配置 %d 项\n", len(req.GetValues()))
	return &pb.ConfigureReply{}, nil
}

func (f *fake) Health(context.Context, *pb.HealthRequest) (*pb.HealthReply, error) {
	if os.Getenv(envMode) == modeUnhealthy {
		return &pb.HealthReply{Ok: false, Message: "按要求报告异常"}, nil
	}
	return &pb.HealthReply{Ok: true}, nil
}

func (f *fake) Shutdown(context.Context, *pb.ShutdownRequest) (*pb.ShutdownReply, error) {
	// 先回复再停服务，否则主程序收不到响应。
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.server.GracefulStop()
	}()
	return &pb.ShutdownReply{}, nil
}

func (f *fake) SendMail(_ context.Context, req *pb.SendMailRequest) (*pb.SendMailReply, error) {
	noteCall("send_mail")
	delay()
	if os.Getenv(envMailFail) != "" {
		return nil, status.Error(codes.Internal, "按要求发信失败")
	}
	return &pb.SendMailReply{}, nil
}

func (f *fake) CreatePayment(_ context.Context, req *pb.CreatePaymentRequest) (*pb.CreatePaymentReply, error) {
	noteCall("create_payment")
	delay()
	return &pb.CreatePaymentReply{
		PayUrl:     "https://example.test/pay/" + req.GetExternalId(),
		GatewayRef: "gw-" + req.GetExternalId(),
	}, nil
}

func (f *fake) QueryPayment(_ context.Context, req *pb.QueryPaymentRequest) (*pb.QueryPaymentReply, error) {
	noteCall("query_payment")
	return &pb.QueryPaymentReply{State: pb.PaymentState_PAYMENT_STATE_PAID}, nil
}

// delay 按 FAKE_HOOK_DELAY 睡一段时间，用于制造超时。
func delay() {
	raw := os.Getenv(envHookDelay)
	if raw == "" {
		return
	}
	ms, err := strconv.Atoi(raw)
	if err != nil {
		return
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

// noteCall 把调用记录追加到文件，供测试断言调用发生的时机。
func noteCall(name string) {
	path := os.Getenv(envCallLog)
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "%s %d\n", name, time.Now().UnixNano())
}
