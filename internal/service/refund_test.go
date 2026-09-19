package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// fakeRefundPlugins 记录 RefundPayment 调用，可注入失败。
type fakeRefundPlugins struct {
	amounts []int64
	extIDs  []string
	err     error
	reply   *pb.RefundPaymentReply
}

func (f *fakeRefundPlugins) RefundPayment(_ context.Context, _ string, req *pb.RefundPaymentRequest) (*pb.RefundPaymentReply, error) {
	// 只记标量字段：拷贝整份 proto 消息会连 sync.Mutex 一起拷（go vet 报错）。
	f.amounts = append(f.amounts, req.GetAmountCents())
	f.extIDs = append(f.extIDs, req.GetExternalId())
	if f.err != nil {
		return nil, f.err
	}
	if f.reply != nil {
		return f.reply, nil
	}
	return &pb.RefundPaymentReply{Ok: true}, nil
}

// seedRefundFixture 是退款测试的公共数据集。
type refundFixture struct {
	svc     *RefundService
	wallet  *WalletService
	plugins *fakeRefundPlugins
	userID  uint
	orderID uint
	payID   uint
	db      *gorm.DB
}

func newRefundFixture(t *testing.T) refundFixture {
	t.Helper()
	db := newTestDB(t)
	plugins := &fakeRefundPlugins{}
	wallet := NewWalletService(db)
	user := seedUser(t, db, "refund-user", 0)
	order := model.Order{
		OrderNo: "RTEST0001", UserID: user.ID, Status: model.OrderPaid,
		TotalCents: 5000,
	}
	if err := db.Create(&order).Error; err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	now := time.Now()
	pay := model.ExternalPayment{
		PluginID: "epay", ExternalID: "ext-refund-1", UserID: user.ID,
		Purpose: model.ExternalPaymentPurposeOrder, TargetID: order.ID,
		AmountCents: 5000, Currency: "CNY", Subject: "测试订单",
		Status: model.ExternalPaymentPaid, PaidAt: &now,
	}
	if err := db.Create(&pay).Error; err != nil {
		t.Fatalf("创建支付意图失败: %v", err)
	}
	svc := NewRefundService(db, plugins, wallet)
	return refundFixture{svc: svc, wallet: wallet, plugins: plugins, userID: user.ID, orderID: order.ID, payID: pay.ID, db: db}
}

// TestRefundAutoApprovedWithinWindow 12 小时窗口内 + 默认策略外全量自动关闭：
// 默认策略（缺省=强制人工）下转人工；AutoApproveAll 时自动退款完成。
func TestRefundPolicyAndAutoExecute(t *testing.T) {
	fx := newRefundFixture(t)

	// 缺省策略（无配置行）= 强制人工：申请停留在 pending。
	item, err := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{
		PaymentID: fx.payID, Reason: "不想要了",
	})
	if err != nil {
		t.Fatalf("提交申请失败: %v", err)
	}
	if item.Status != model.RefundPending || item.PolicyResult != model.RefundPolicyManualReview {
		t.Fatalf("缺省策略应转人工，实际 %s/%s", item.Status, item.PolicyResult)
	}

	// 管理员通过 → 执行渠道退款 + 完成终态。
	got, err := fx.svc.Review(context.Background(), 99, item.ID, RefundReviewInput{Approve: true})
	if err != nil {
		t.Fatalf("审批失败: %v", err)
	}
	if got.Status != model.RefundCompleted {
		t.Fatalf("渠道退款成功后应 refunded，实际 %s", got.Status)
	}
	if len(fx.plugins.amounts) != 1 || fx.plugins.amounts[0] != 5000 {
		t.Fatalf("应向渠道退 5000 分，实际 %v", fx.plugins.amounts)
	}

	// 重复审批应冲突。
	if _, err := fx.svc.Review(context.Background(), 99, item.ID, RefundReviewInput{Approve: true}); err == nil {
		t.Fatal("重复审批应返回冲突")
	}
}

// TestRefundAutoApproveAll 自动通过策略：提交即完成，渠道+余额各退各的。
func TestRefundAutoApproveAll(t *testing.T) {
	fx := newRefundFixture(t)
	if _, err := fx.svc.SavePolicy(RefundPolicyInput{AutoApproveAll: true}); err != nil {
		t.Fatalf("保存策略失败: %v", err)
	}
	item, err := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{
		PaymentID: fx.payID, Reason: "开不出来",
	})
	if err != nil {
		t.Fatalf("提交申请失败: %v", err)
	}
	if item.Status != model.RefundCompleted || item.PolicyResult != model.RefundPolicyAutoApproved {
		t.Fatalf("自动通过应直接完成，实际 %s/%s", item.Status, item.PolicyResult)
	}
	if len(fx.plugins.amounts) != 1 {
		t.Fatalf("应恰好调用一次渠道退款，实际 %d", len(fx.plugins.amounts))
	}
}

// TestRefundPolicyDenyAfterHours 购买满 N 小时不予退款：直接拒绝创建。
func TestRefundPolicyDenyAfterHours(t *testing.T) {
	fx := newRefundFixture(t)
	if _, err := fx.svc.SavePolicy(RefundPolicyInput{AutoApproveAll: true, NoRefundAfterHours: 1}); err != nil {
		t.Fatalf("保存策略失败: %v", err)
	}
	// 支付时间拨回 2 小时前。
	past := time.Now().Add(-2 * time.Hour)
	if err := fx.db.Model(&model.ExternalPayment{}).Where("id = ?", fx.payID).
		Update("paid_at", past).Error; err != nil {
		t.Fatalf("回拨支付时间失败: %v", err)
	}
	if _, err := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{
		PaymentID: fx.payID, Reason: "超时退款",
	}); err == nil {
		t.Fatal("超过不退时限应拒绝创建")
	}
}

// TestRefundChannelFailureKeepsFailed 渠道失败 → failed + FailReason，可重试成功。
func TestRefundChannelFailureKeepsFailed(t *testing.T) {
	fx := newRefundFixture(t)
	// 全量自动通过，绕开人工审批；渠道返回拒绝。
	if _, err := fx.svc.SavePolicy(RefundPolicyInput{AutoApproveAll: true}); err != nil {
		t.Fatalf("保存策略失败: %v", err)
	}
	fx.plugins.reply = &pb.RefundPaymentReply{Ok: false, Error: "商户未开通退款"}

	item, err := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{
		PaymentID: fx.payID, Reason: "测试失败",
	})
	if err != nil {
		t.Fatalf("提交申请失败: %v", err)
	}
	if item.Status != model.RefundFailed {
		t.Fatalf("渠道拒绝应落 failed，实际 %s", item.Status)
	}
	// 余额不应入账（防止渠道 + 余额双份到手）。
	overview, _ := fx.wallet.Overview(fx.userID)
	if overview.BalanceCents != 0 {
		t.Fatalf("渠道失败时余额不应入账，实际 %d", overview.BalanceCents)
	}

	// 渠道恢复后重试成功。
	fx.plugins.reply = nil
	got, err := fx.svc.RetryFailed(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("重试失败: %v", err)
	}
	if got.Status != model.RefundCompleted {
		t.Fatalf("重试成功应 refunded，实际 %s", got.Status)
	}
}

// TestRefundPluginUnavailableFails 插件调用错误（如 UNIMPLEMENTED）同样落 failed。
func TestRefundPluginUnavailableFails(t *testing.T) {
	fx := newRefundFixture(t)
	if _, err := fx.svc.SavePolicy(RefundPolicyInput{AutoApproveAll: true}); err != nil {
		t.Fatalf("保存策略失败: %v", err)
	}
	fx.plugins.err = errors.New("rpc error: code = Unimplemented")

	item, err := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{
		PaymentID: fx.payID, Reason: "测试",
	})
	if err != nil {
		t.Fatalf("提交申请失败: %v", err)
	}
	if item.Status != model.RefundFailed || item.FailReason == "" {
		t.Fatalf("插件不可用应落 failed 且带原因，实际 %s/%s", item.Status, item.FailReason)
	}
}

// TestRefundDuplicatePending 重复提交被拒。
func TestRefundDuplicatePending(t *testing.T) {
	fx := newRefundFixture(t)
	if _, err := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{
		PaymentID: fx.payID, Reason: "第一次",
	}); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if _, err := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{
		PaymentID: fx.payID, Reason: "第二次",
	}); err == nil {
		t.Fatal("重复提交应被拒绝")
	}
}

// TestRefundCancelPending 用户撤回待审申请。
func TestRefundCancelPending(t *testing.T) {
	fx := newRefundFixture(t)
	item, err := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{
		PaymentID: fx.payID, Reason: "撤回测试",
	})
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	got, err := fx.svc.Cancel(fx.userID, item.ID)
	if err != nil {
		t.Fatalf("撤回失败: %v", err)
	}
	if got.Status != model.RefundCanceled {
		t.Fatalf("撤回后应 canceled，实际 %s", got.Status)
	}
}

// TestRefundRejectRequiresRemark 驳回必须填写原因。
func TestRefundRejectRequiresRemark(t *testing.T) {
	fx := newRefundFixture(t)
	item, _ := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{
		PaymentID: fx.payID, Reason: "驳回测试",
	})
	if _, err := fx.svc.Review(context.Background(), 99, item.ID, RefundReviewInput{Approve: false}); err == nil {
		t.Fatal("无备注驳回应被拒绝")
	}
	got, err := fx.svc.Review(context.Background(), 99, item.ID, RefundReviewInput{Approve: false, Remark: "不符合条件"})
	if err != nil {
		t.Fatalf("驳回失败: %v", err)
	}
	if got.Status != model.RefundRejected {
		t.Fatalf("驳回后应 rejected，实际 %s", got.Status)
	}
}
