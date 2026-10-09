package service

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"
)

// The real Manager launches a copy of the test executable as a token-checked
// gRPC plugin. Its payment calls proxy only to a loopback test server: no gateway
// or credentials are involved. This exercises the production process/RPC seam.
func init() {
	token := os.Getenv(plugin.EnvToken)
	name := filepath.Base(os.Args[0])
	if token == "" || (name != "plugin" && name != "plugin.exe") {
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get(plugin.MetadataToken)
		if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(token)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "test plugin token required")
		}
		return next(ctx, req)
	}))
	pb.RegisterPluginServer(server, &contractPaymentProxy{server: server})
	line, _ := json.Marshal(map[string]int{"port": listener.Addr().(*net.TCPAddr).Port})
	fmt.Println(string(line))
	if err := server.Serve(listener); err != nil {
		panic(err)
	}
	os.Exit(0)
}

type contractPaymentProxy struct {
	pb.UnimplementedPluginServer
	server *grpc.Server
	client pb.PluginClient
}

func (*contractPaymentProxy) Describe(context.Context, *pb.DescribeRequest) (*pb.Manifest, error) {
	return &pb.Manifest{Name: "Local money-contract test plugin", Capabilities: []pb.Capability{pb.Capability_CAPABILITY_CREATE_PAYMENT}}, nil
}
func (p *contractPaymentProxy) Configure(_ context.Context, r *pb.ConfigureRequest) (*pb.ConfigureReply, error) {
	conn, err := grpc.NewClient(r.Values["backend"], grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	p.client = pb.NewPluginClient(conn)
	return &pb.ConfigureReply{}, nil
}
func (*contractPaymentProxy) Health(context.Context, *pb.HealthRequest) (*pb.HealthReply, error) {
	return &pb.HealthReply{Ok: true}, nil
}
func (p *contractPaymentProxy) Shutdown(context.Context, *pb.ShutdownRequest) (*pb.ShutdownReply, error) {
	go p.server.GracefulStop()
	return &pb.ShutdownReply{}, nil
}
func (p *contractPaymentProxy) CreatePayment(ctx context.Context, r *pb.CreatePaymentRequest) (*pb.CreatePaymentReply, error) {
	return p.client.CreatePayment(ctx, r)
}
func (p *contractPaymentProxy) QueryPayment(ctx context.Context, r *pb.QueryPaymentRequest) (*pb.QueryPaymentReply, error) {
	return p.client.QueryPayment(ctx, r)
}

type contractPaymentGateway struct {
	pb.UnimplementedPluginServer
	creates atomic.Int32
	create  func(context.Context, *pb.CreatePaymentRequest) (*pb.CreatePaymentReply, error)
	query   func(context.Context, *pb.QueryPaymentRequest) (*pb.QueryPaymentReply, error)
}

func (g *contractPaymentGateway) CreatePayment(ctx context.Context, r *pb.CreatePaymentRequest) (*pb.CreatePaymentReply, error) {
	g.creates.Add(1)
	if g.create != nil {
		return g.create(ctx, r)
	}
	return &pb.CreatePaymentReply{PayUrl: "https://example.test/pay/" + r.ExternalId, GatewayRef: "gw-" + r.ExternalId}, nil
}
func (g *contractPaymentGateway) QueryPayment(ctx context.Context, r *pb.QueryPaymentRequest) (*pb.QueryPaymentReply, error) {
	if g.query != nil {
		return g.query(ctx, r)
	}
	return &pb.QueryPaymentReply{State: pb.PaymentState_PAYMENT_STATE_PENDING}, nil
}

type contractPaymentConfig struct{ backend string }

func (c contractPaymentConfig) PluginConfig(string) (map[string]string, error) {
	return map[string]string{"backend": c.backend}, nil
}
func (contractPaymentConfig) PluginScopes(string) ([]string, error) { return nil, nil }

func newContractPaymentService(t *testing.T, db *gorm.DB, gateway *contractPaymentGateway) (*PaymentService, model.PaymentMethod) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterPluginServer(server, gateway)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	dataDir := t.TempDir()
	dir := filepath.Join(plugin.Root(dataDir), "money-contract")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	name := "plugin"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), binary, 0700); err != nil {
		t.Fatal(err)
	}
	manager := plugin.NewManager(dataDir, "http://127.0.0.1:0/api/plugin/v1", contractPaymentConfig{listener.Addr().String()}, nil, t.Logf)
	t.Cleanup(manager.Close)
	if err := manager.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Enable(context.Background(), "money-contract"); err != nil {
		t.Fatal(err)
	}
	inst, err := manager.Get("money-contract")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for inst.Snapshot().State != plugin.StateRunning {
		if time.Now().After(deadline) {
			t.Fatalf("test plugin startup: %+v", inst.Snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
	method := model.PaymentMethod{Name: "Local contract", PluginID: "money-contract", Enabled: true}
	if err := db.Create(&method).Error; err != nil {
		t.Fatal(err)
	}
	wallet := NewWalletService(db)
	orders := NewOrderService(db, nil, wallet, nil)
	return NewPaymentService(db, manager, wallet, orders, NewBillingService(db, wallet, nil)), method
}

func TestPaymentConcurrentOrderInvoiceAliasesHaveOneChannel(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "concurrent-channel", 5000)
	product := seedProduct(t, db, "contract-local", 1000)
	orders := NewOrderService(db, nil, NewWalletService(db), nil)
	order, err := orders.CreateDirect(user.ID, []OrderLine{{ProductID: product.ID, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var invoice model.Invoice
	if err := db.First(&invoice, "order_id = ?", order.ID).Error; err != nil {
		t.Fatal(err)
	}
	gateway := &contractPaymentGateway{}
	payments, method := newContractPaymentService(t, db, gateway)
	// Hold both preflight method reads before either can reserve a target. A
	// transaction-local fence, not a preflight query, must pick the sole winner.
	var arrived atomic.Int32
	ready := make(chan struct{})
	if err := db.Callback().Query().After("gorm:query").Register("contract_preflight", func(tx *gorm.DB) {
		if tx.Statement.Table != "payment_methods" {
			return
		}
		if arrived.Add(1) == 2 {
			close(ready)
		}
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			tx.AddError(fmt.Errorf("preflight rendezvous timed out"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove("contract_preflight") })
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, in := range []PaymentCreateInput{
		{Purpose: model.ExternalPaymentPurposeOrder, TargetID: order.ID, PluginID: fmt.Sprint(method.ID)},
		{Purpose: model.ExternalPaymentPurposeInvoice, TargetID: invoice.ID, PluginID: fmt.Sprint(method.ID)},
	} {
		wg.Add(1)
		go func(i int, in PaymentCreateInput) {
			defer wg.Done()
			_, errs[i] = payments.Create(context.Background(), user.ID, "127.0.0.1", in)
		}(i, in)
	}
	wg.Wait()
	winners := 0
	for _, err := range errs {
		if err == nil {
			winners++
		}
	}
	if winners != 1 || gateway.creates.Load() != 1 {
		t.Fatalf("one canonical channel must win, winners=%d RPCs=%d errors=%v", winners, gateway.creates.Load(), errs)
	}
	var intents []model.ExternalPayment
	if err := db.Find(&intents).Error; err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || balanceOf(t, db, user.ID) != 5000 {
		t.Fatalf("reservation must leave exactly one intent and no wallet debit: %d", len(intents))
	}
	if !db.Migrator().HasIndex(&model.ExternalPayment{}, "idx_external_payment_active_target") {
		t.Fatal("canonical target must have a nullable unique database index")
	}
	var key string
	if err := db.Model(&model.ExternalPayment{}).Where("id = ?", intents[0].ID).Pluck("active_target_key", &key).Error; err != nil {
		t.Fatal(err)
	}
	if key != fmt.Sprintf("order:%d", order.ID) {
		t.Fatalf("noncanonical key: %q", key)
	}
}
