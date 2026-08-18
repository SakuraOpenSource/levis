package notify

import (
	"context"
	"sync"
	"testing"
	"time"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// fakeMailer 记录发信调用，用于测试。
type fakeMailer struct {
	mu    sync.Mutex
	calls []fakeCall
	delay time.Duration
}

type fakeCall struct {
	to      string
	subject string
	body    string
}

func (f *fakeMailer) SendMail(_ context.Context, req *pb.SendMailRequest) error {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var to string
	if len(req.To) > 0 {
		to = req.To[0].GetAddress()
	}
	f.calls = append(f.calls, fakeCall{to: to, subject: req.GetSubject(), body: req.GetTextBody()})
	return nil
}

func (f *fakeMailer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeMailer) lastCall() fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return fakeCall{}
	}
	return f.calls[len(f.calls)-1]
}

// fakeResolver 造测试用的收件人。
type fakeResolver struct {
	site string
}

func (r fakeResolver) Recipient(userID uint) (Recipient, error) {
	return Recipient{Name: "测试用户", Email: "user@example.com"}, nil
}

func (r fakeResolver) SiteName() string {
	if r.site == "" {
		return "测试站"
	}
	return r.site
}

// TestNotifierEnqueuesAndDelivers 确认投递是异步的，且最终送达。
func TestNotifierEnqueuesAndDelivers(t *testing.T) {
	mailer := &fakeMailer{delay: 50 * time.Millisecond}
	n := New(mailer, fakeResolver{}, t.Logf)
	defer n.Close()

	// 投递立刻返回，不等发信完成。
	start := time.Now()
	n.TicketReplied(1, "TKT-001", "测试工单", "客服回复内容")
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Errorf("投递应立刻返回，实际耗时 %v", elapsed)
	}

	// 稍等片刻，信应该发出去了。
	time.Sleep(100 * time.Millisecond)
	if got := mailer.callCount(); got != 1 {
		t.Fatalf("应发出 1 封信，实际 %d 封", got)
	}

	call := mailer.lastCall()
	if call.to != "user@example.com" {
		t.Errorf("收件人应为 user@example.com，实际 %q", call.to)
	}
	if call.subject != "[测试站] 工单 TKT-001 有新回复" {
		t.Errorf("标题不符，实际 %q", call.subject)
	}
	if call.body == "" || len(call.body) < 10 {
		t.Errorf("正文应有内容，实际 %q", call.body)
	}
}

// TestNotifierDropsWhenFull 确认队列满时丢弃而不是阻塞。
func TestNotifierDropsWhenFull(t *testing.T) {
	mailer := &fakeMailer{delay: 200 * time.Millisecond} // 慢但不会永远卡住
	n := New(mailer, fakeResolver{}, t.Logf)
	defer n.Close()

	// 塞满队列再多塞几封，全都应立刻返回。
	for i := 0; i < queueSize+10; i++ {
		start := time.Now()
		n.OrderPaid(uint(i), "ORD-001", 10000)
		if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
			t.Errorf("第 %d 次投递应立刻返回，实际耗时 %v", i, elapsed)
		}
	}
	// worker 在慢慢消费，队列应该丢弃了超出部分。
	time.Sleep(50 * time.Millisecond)
	if got := mailer.callCount(); got > 2 {
		// 可能发出了 1 或 2 封，但不会很多。
		t.Errorf("worker 应还在消费前几封，实际已发 %d 封", got)
	}
}

// TestNotifierNilIsSafe 确认 nil 的 *Notifier 上调用任何方法都不 panic。
func TestNotifierNilIsSafe(t *testing.T) {
	var n *Notifier
	n.TicketReplied(1, "TKT-001", "主题", "正文")
	n.KYCReviewed(1, true, "")
	n.OrderPaid(1, "ORD-001", 10000)
	n.Close()
}

// TestNotifierSkipsEmptyEmail 确认邮箱为空时不调插件。
func TestNotifierSkipsEmptyEmail(t *testing.T) {
	mailer := &fakeMailer{}
	emptyResolver := emptyEmailResolver{}

	n := New(mailer, emptyResolver, t.Logf)
	defer n.Close()

	n.TicketReplied(1, "TKT-001", "主题", "正文")
	time.Sleep(50 * time.Millisecond)

	if got := mailer.callCount(); got != 0 {
		t.Errorf("邮箱为空时不应发信，实际发了 %d 封", got)
	}
}

// emptyEmailResolver 返回空邮箱。
type emptyEmailResolver struct{}

func (emptyEmailResolver) Recipient(uint) (Recipient, error) {
	return Recipient{Name: "无邮箱用户", Email: ""}, nil
}

func (emptyEmailResolver) SiteName() string {
	return "测试站"
}

// TestKYCReviewedApprovedAndRejected 确认通过与驳回的文案不同。
func TestKYCReviewedApprovedAndRejected(t *testing.T) {
	mailer := &fakeMailer{}
	n := New(mailer, fakeResolver{}, t.Logf)
	defer n.Close()

	n.KYCReviewed(1, true, "")
	time.Sleep(50 * time.Millisecond)
	approved := mailer.lastCall()
	if approved.subject != "[测试站] 实名认证已通过" {
		t.Errorf("通过时标题不符，实际 %q", approved.subject)
	}

	n.KYCReviewed(2, false, "证件照模糊")
	time.Sleep(50 * time.Millisecond)
	rejected := mailer.lastCall()
	if rejected.subject != "[测试站] 实名认证未通过" {
		t.Errorf("驳回时标题不符，实际 %q", rejected.subject)
	}
	if rejected.body == "" || !contains(rejected.body, "证件照模糊") {
		t.Errorf("驳回原因应出现在正文里，实际 %q", rejected.body)
	}
}

// TestOrderPaidFormatsAmount 确认金额格式化正确。
func TestOrderPaidFormatsAmount(t *testing.T) {
	mailer := &fakeMailer{}
	n := New(mailer, fakeResolver{}, t.Logf)
	defer n.Close()

	n.OrderPaid(1, "ORD-123", 12345)
	time.Sleep(50 * time.Millisecond)
	call := mailer.lastCall()

	if !contains(call.body, "¥123.45") {
		t.Errorf("正文应包含格式化后的金额 ¥123.45，实际 %q", call.body)
	}
	if !contains(call.body, "ORD-123") {
		t.Errorf("正文应包含订单号 ORD-123，实际 %q", call.body)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (s[0:len(sub)] == sub || contains(s[1:], sub))))
}
