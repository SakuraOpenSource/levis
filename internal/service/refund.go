package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// 无理由退款窗口：付款后 12 小时内可无条件退款。
const refundNoReasonHours = 12

// RefundService 处理退款申请、策略判定与退款执行。
//
// 资金安全约定：
//   - 渠道退款与余额退还是资金出口，必须先 CAS 认领申请（pending/failed →
//     approved）再执行，任何失败都可安全重试；
//   - 渠道退款失败不回滚审批状态（保持 failed），管理员可一键重试；
//   - 余额退还是入账（方向为 +），天然幂等由 RefundNo 关联的流水语义保证，
//     渠道失败时不入余额，避免"渠道 + 余额"双份到手。
type RefundService struct {
	db      *gorm.DB
	plugins pluginRefundDispatcher
	wallet  *WalletService
}

// pluginRefundDispatcher 是退款执行需要的最小插件能力（便于测试注入）。
type pluginRefundDispatcher interface {
	RefundPayment(ctx context.Context, id string, req *pb.RefundPaymentRequest) (*pb.RefundPaymentReply, error)
}

// NewRefundService 构造 RefundService。
func NewRefundService(db *gorm.DB, plugins pluginRefundDispatcher, wallet *WalletService) *RefundService {
	return &RefundService{db: db, plugins: plugins, wallet: wallet}
}

// RefundPolicyInput 是管理员保存的退款策略入参。
type RefundPolicyInput struct {
	ForceManual        bool `json:"force_manual"`
	AutoApproveAll     bool `json:"auto_approve_all"`
	NoRefundAfterHours int  `json:"no_refund_after_hours"`
}

// Policy 读取当前退款策略；配置缺失时等价于「强制人工」。
// 读取失败同样回退到强制人工：资金出口宁可多审，不可误放。
func (s *RefundService) Policy() (model.RefundPolicyConfig, error) {
	policy := model.RefundPolicyConfig{ForceManual: true}
	var row model.Setting
	if err := s.db.First(&row, "key = ?", model.SettingRefundPolicy).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return policy, nil
		}
		return policy, err
	}
	if err := json.Unmarshal([]byte(row.Value), &policy); err != nil {
		return model.RefundPolicyConfig{ForceManual: true}, nil
	}
	return policy, nil
}

// SavePolicy 保存退款策略（管理员）。
func (s *RefundService) SavePolicy(in RefundPolicyInput) (model.RefundPolicyConfig, error) {
	if in.NoRefundAfterHours < 0 {
		return model.RefundPolicyConfig{}, ErrBadRequest("不退时长不能为负数")
	}
	policy := model.RefundPolicyConfig{
		ForceManual:        in.ForceManual,
		AutoApproveAll:     in.AutoApproveAll,
		NoRefundAfterHours: in.NoRefundAfterHours,
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return model.RefundPolicyConfig{}, err
	}
	row := model.Setting{Key: model.SettingRefundPolicy, Value: string(raw)}
	if err := s.db.Create(&row).Error; err != nil {
		return model.RefundPolicyConfig{}, err
	}
	return policy, nil
}

// RefundCreateInput 是用户提交退款申请的入参。
//
// 推荐传 ServiceID（选择已开通的产品）；传了 ServiceID 时订单与支付记录
// 自动解析，OrderID/PaymentID 仅作兜底（历史兼容，不推荐前端使用）。
type RefundCreateInput struct {
	ServiceID uint   `json:"service_id"`
	PaymentID uint   `json:"payment_id"`
	OrderID   uint   `json:"order_id"`
	Reason    string `json:"reason"`
}

// refundNo 是对外展示的退款单号。
func refundNo() (string, error) {
	stamp := time.Now().UTC().Format("20060102150405")
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("R%s%s", stamp, strings.ToUpper(hex.EncodeToString(buf))), nil
}

// Create 提交退款申请。
//
// 判定顺序：申请资格（已支付、归属、金额上限、重复申请）→ 策略判定
// （拒绝 / 人工 / 自动通过）。判定为自动通过时当场执行退款；否则停留在
// pending 等待管理员处理。
func (s *RefundService) Create(ctx context.Context, userID uint, in RefundCreateInput) (*model.RefundRequest, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return nil, ErrBadRequest("请填写退款原因")
	}
	if in.ServiceID == 0 && in.PaymentID == 0 && in.OrderID == 0 {
		return nil, ErrBadRequest("请选择要退款的产品")
	}

	// 按产品申请：服务 → 订单（ServiceID 的来源），支付记录随后按订单解析。
	if in.ServiceID != 0 {
		var svc model.Service
		if err := s.db.First(&svc, "id = ? AND user_id = ?", in.ServiceID, userID).Error; err != nil {
			return nil, ErrNotFound("产品不存在")
		}
		if in.OrderID == 0 {
			in.OrderID = svc.OrderID
		}
		if in.OrderID == 0 {
			return nil, ErrBadRequest("该产品没有关联订单，请联系管理员处理")
		}
	}

	// 锁定原支付意图（可选：余额支付订单没有意图）。
	var payment *model.ExternalPayment
	if in.PaymentID != 0 {
		item, err := s.payablePayment(userID, in.PaymentID)
		if err != nil {
			return nil, err
		}
		payment = item
	}

	// 解析关联订单（用于时长策略与展示）。
	var order model.Order
	switch {
	case payment != nil && payment.TargetID != 0 && payment.Purpose == model.ExternalPaymentPurposeOrder:
		if err := s.db.First(&order, payment.TargetID).Error; err != nil {
			return nil, ErrNotFound("订单不存在")
		}
		if order.UserID != userID {
			return nil, ErrNotFound("订单不存在")
		}
	case in.OrderID != 0:
		if err := s.db.First(&order, in.OrderID).Error; err != nil {
			return nil, ErrNotFound("订单不存在")
		}
		if order.UserID != userID {
			return nil, ErrNotFound("订单不存在")
		}
	default:
		return nil, ErrBadRequest("请选择要退款的产品")
	}

	// 按订单自动补找已完成的支付记录（按产品申请时 ServiceID→OrderID 走到这）：
	// 一个订单可能有多笔支付（失败重付），取最近一笔已完成的。
	if payment == nil {
		var pay model.ExternalPayment
		err := s.db.Where("user_id = ? AND target_id = ? AND purpose = ? AND status = ?",
			userID, order.ID, model.ExternalPaymentPurposeOrder, model.ExternalPaymentPaid).
			Order("id DESC").First(&pay).Error
		if err == nil {
			payment = &pay
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		// 没有支付记录 = 纯余额支付订单，金额按订单总额退余额。
	}

	// 已有进行中的申请则拒绝重复提交。
	var dup int64
	query := s.db.Model(&model.RefundRequest{}).Where("user_id = ? AND status IN ?", userID,
		[]string{model.RefundPending, model.RefundApproved})
	if payment != nil {
		query = query.Where("payment_id = ?", payment.ID)
	} else {
		query = query.Where("order_id = ?", order.ID)
	}
	if err := query.Count(&dup).Error; err != nil {
		return nil, err
	}
	if dup > 0 {
		return nil, ErrConflict("该笔支付已有处理中的退款申请")
	}

	// 退款上限 = 实收金额（渠道意图金额 + 余额抵扣），只能全退不部分退：
	// 部分退款会让"已用服务 + 部分回款"的账难以对平，第一期不支持。
	var paidCents int64
	if payment != nil {
		paidCents = payment.AmountCents + payment.BalanceCents
	} else {
		paidCents = order.TotalCents
	}
	if paidCents <= 0 {
		return nil, ErrBadRequest("该订单没有可退的支付金额")
	}

	// 策略判定。
	policy, err := s.Policy()
	if err != nil {
		return nil, err
	}
	paidAt := time.Now()
	if payment != nil && payment.PaidAt != nil {
		paidAt = *payment.PaidAt
	} else if order.PaidAt != nil {
		paidAt = *order.PaidAt
	}
	policyResult := s.evaluatePolicy(policy, paidAt)
	if policyResult == model.RefundPolicyDenied {
		return nil, ErrBadRequest("已超过可退款时限，不符合退款条件")
	}

	// 金额拆分：渠道退意图金额，余额退当初抵扣的部分。
	var channelCents, balanceCents int64
	if payment != nil {
		channelCents, balanceCents = payment.AmountCents, payment.BalanceCents
	} else {
		// 余额支付的订单：全额退回余额。
		channelCents, balanceCents = 0, paidCents
	}

	no, err := refundNo()
	if err != nil {
		return nil, err
	}
	item := model.RefundRequest{
		RefundNo:     no,
		UserID:       userID,
		ServiceID:    in.ServiceID,
		PaymentID:    mapUint(payment, payment),
		OrderID:      order.ID,
		AmountCents:  paidCents,
		ChannelCents: channelCents,
		BalanceCents: balanceCents,
		Reason:       truncateRunes(reason, 500),
		Status:       model.RefundPending,
		PolicyResult: policyResult,
	}
	if err := s.db.Create(&item).Error; err != nil {
		return nil, err
	}

	// 自动通过：立即执行；执行失败不回滚申请（状态已落 failed，管理员可
	// 在后台重试），对用户表现为「退款处理中/失败」而非提交被拒。
	if policyResult == model.RefundPolicyAutoApproved {
		_ = s.execute(context.Background(), &item)
		// execute 直接写库；回读保证返回给调用方的状态是最新终态。
		if err := s.db.First(&item, item.ID).Error; err != nil {
			return nil, err
		}
	}
	return &item, nil
}

// mapUint 消除 payment 为 nil 时的歧义，返回其 ID。
func mapUint(payment *model.ExternalPayment, _ any) uint {
	if payment == nil {
		return 0
	}
	return payment.ID
}

// payablePayment 校验支付意图归属且已支付。
func (s *RefundService) payablePayment(userID, paymentID uint) (*model.ExternalPayment, error) {
	var item model.ExternalPayment
	if err := s.db.First(&item, paymentID).Error; err != nil {
		return nil, ErrNotFound("支付记录不存在")
	}
	if item.UserID != userID {
		return nil, ErrNotFound("支付记录不存在")
	}
	if item.Status != model.ExternalPaymentPaid {
		return nil, ErrBadRequest("仅已完成的支付可以申请退款")
	}
	return &item, nil
}

// evaluatePolicy 返回策略判定结果。
func (s *RefundService) evaluatePolicy(policy model.RefundPolicyConfig, paidAt time.Time) string {
	// 「购买满 N 小时不予退款」：命中直接拒绝，优先级最高。
	if policy.NoRefundAfterHours > 0 && time.Since(paidAt) > time.Duration(policy.NoRefundAfterHours)*time.Hour {
		return model.RefundPolicyDenied
	}
	if policy.ForceManual {
		return model.RefundPolicyManualReview
	}
	if policy.AutoApproveAll {
		return model.RefundPolicyAutoApproved
	}
	// 默认：12 小时无理由窗口内自动通过，之外转人工。
	if time.Since(paidAt) <= refundNoReasonHours*time.Hour {
		return model.RefundPolicyAutoApproved
	}
	return model.RefundPolicyManualReview
}

// Cancel 用户撤回待审的申请。
func (s *RefundService) Cancel(userID, id uint) (*model.RefundRequest, error) {
	res := s.db.Model(&model.RefundRequest{}).
		Where("id = ? AND user_id = ? AND status = ?", id, userID, model.RefundPending).
		Updates(map[string]any{"status": model.RefundCanceled, "reviewed_at": time.Now()})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrNotFound("退款申请不存在或不可撤回")
	}
	var item model.RefundRequest
	if err := s.db.First(&item, id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// List 用户自己的退款申请列表。
func (s *RefundService) List(userID uint, offset, limit int) ([]model.RefundRequest, int64, error) {
	var total int64
	if err := s.db.Model(&model.RefundRequest{}).Where("user_id = ?", userID).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []model.RefundRequest
	if err := s.db.Where("user_id = ?", userID).
		Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// AdminList 管理员分页查看全部申请，status 可选过滤。
func (s *RefundService) AdminList(status string, offset, limit int) ([]model.RefundRequest, int64, error) {
	query := s.db.Model(&model.RefundRequest{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []model.RefundRequest
	if err := query.Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// RefundReviewInput 是管理员审批入参。approve=false 时 Remark 必填。
type RefundReviewInput struct {
	Approve bool   `json:"approve"`
	Remark  string `json:"remark"`
}

// Review 管理员审批：通过则立即执行退款，驳回则记录原因。
// 状态迁移用 CAS（pending → approved/rejected），并发审批只有一方生效。
// 通过后的退款执行由 execute 完成（此时状态已是 approved）。
func (s *RefundService) Review(ctx context.Context, reviewerID, id uint, in RefundReviewInput) (*model.RefundRequest, error) {
	var item model.RefundRequest
	if err := s.db.First(&item, id).Error; err != nil {
		return nil, ErrNotFound("退款申请不存在")
	}
	if item.Status != model.RefundPending {
		return nil, ErrConflict("该申请已处理，请刷新后查看")
	}
	remark := strings.TrimSpace(in.Remark)
	if !in.Approve && remark == "" {
		return nil, ErrBadRequest("驳回时必须填写原因")
	}

	status := model.RefundRejected
	if in.Approve {
		status = model.RefundApproved
	}
	now := time.Now()
	res := s.db.Model(&model.RefundRequest{}).
		Where("id = ? AND status = ?", id, model.RefundPending).
		Updates(map[string]any{
			"status": status, "review_remark": truncateRunes(remark, 500),
			"reviewer_id": reviewerID, "reviewed_at": now,
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrConflict("该申请已处理，请刷新后查看")
	}

	if err := s.db.First(&item, id).Error; err != nil {
		return nil, err
	}
	if in.Approve {
		// 审批通过即执行；渠道失败落 failed，管理员可重试。
		if err := s.execute(ctx, &item); err != nil {
			return nil, err
		}
		// execute 直接写库；回读保证返回给调用方的状态是最新终态。
		if err := s.db.First(&item, id).Error; err != nil {
			return nil, err
		}
	}
	return &item, nil
}

// RetryFailed 对渠道退款失败的申请重新执行（管理员）。
func (s *RefundService) RetryFailed(ctx context.Context, id uint) (*model.RefundRequest, error) {
	var item model.RefundRequest
	if err := s.db.First(&item, id).Error; err != nil {
		return nil, ErrNotFound("退款申请不存在")
	}
	if item.Status != model.RefundFailed {
		return nil, ErrConflict("仅渠道退款失败的申请可以重试")
	}
	if err := s.execute(ctx, &item); err != nil {
		return nil, err
	}
	// execute 直接写库；回读保证返回给调用方的状态是最新终态。
	if err := s.db.First(&item, id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// execute 执行退款：渠道退款（如有）→ 余额退回（如有）→ 终态 refunded。
//
// 幂等与失败语义：
//   - 认领 CAS（pending/approved/failed → approved）防止并发重复执行；
//   - 渠道失败：状态落 failed + FailReason，余额不入账（用户可重试）；
//   - 渠道无渠道侧退款能力（插件 UNIMPLEMENTED / 无支付方式配置）时：
//     若金额全部走余额退还则继续，否则落 failed 提示管理员线下处理。
func (s *RefundService) execute(ctx context.Context, item *model.RefundRequest) error {
	// 认领：自动通过路径从 pending 进入，管理员审批/重试路径从 approved/failed 进入。
	res := s.db.Model(&model.RefundRequest{}).
		Where("id = ? AND status IN ?", item.ID,
			[]string{model.RefundPending, model.RefundApproved, model.RefundFailed}).
		Updates(map[string]any{"status": model.RefundApproved, "fail_reason": ""})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrConflict("退款申请状态已变化，请刷新后查看")
	}

	var failed error
	if item.ChannelCents > 0 {
		failed = s.refundChannel(ctx, item)
	}
	if failed == nil && item.BalanceCents > 0 {
		failed = s.refundBalance(item)
	}
	if failed != nil {
		s.db.Model(&model.RefundRequest{}).Where("id = ?", item.ID).
			Updates(map[string]any{"status": model.RefundFailed, "fail_reason": truncateRunes(failed.Error(), 500)})
		return ErrConflict("退款执行失败: %v", failed)
	}

	now := time.Now()
	return s.db.Model(&model.RefundRequest{}).Where("id = ?", item.ID).
		Updates(map[string]any{"status": model.RefundCompleted, "refunded_at": now}).Error
}

// refundChannel 调用支付插件向渠道退款。
func (s *RefundService) refundChannel(ctx context.Context, item *model.RefundRequest) error {
	if s.plugins == nil {
		return fmt.Errorf("支付插件不可用")
	}
	var payment model.ExternalPayment
	if err := s.db.First(&payment, item.PaymentID).Error; err != nil {
		return fmt.Errorf("原支付记录不存在")
	}
	if payment.Status != model.ExternalPaymentPaid {
		return fmt.Errorf("原支付未完成，渠道无法退款")
	}
	// 按支付方式取配置；旧数据回退到该插件的第一个启用方式。
	cfg := map[string]string{}
	if payment.PaymentMethodID != nil {
		var method model.PaymentMethod
		if err := s.db.First(&method, *payment.PaymentMethodID).Error; err == nil {
			cfg = parsePaymentMethodConfig(method.Config)
		}
	} else {
		var method model.PaymentMethod
		if err := s.db.Where("plugin_id = ? AND enabled = ?", payment.PluginID, true).
			Order("sort_order ASC, id ASC").First(&method).Error; err == nil {
			cfg = parsePaymentMethodConfig(method.Config)
		}
	}
	reply, err := s.plugins.RefundPayment(ctx, payment.PluginID, &pb.RefundPaymentRequest{
		ExternalId:  payment.ExternalID,
		GatewayRef:  payment.GatewayRef,
		AmountCents: item.ChannelCents,
		Config:      cfg,
	})
	if err != nil {
		return fmt.Errorf("渠道退款调用失败: %v", err)
	}
	if !reply.GetOk() {
		return fmt.Errorf("渠道拒绝退款: %s", reply.GetError())
	}
	return nil
}

// refundBalance 把余额部分退回用户钱包。
func (s *RefundService) refundBalance(item *model.RefundRequest) error {
	_, err := s.wallet.Recharge(item.UserID, item.BalanceCents)
	return err
}

// truncateRunes 按 rune 截断，避免中文被切半。
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
