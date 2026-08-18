// Package notify 把站内事件异步投递给邮件插件。
//
// 三条设计约束，都是为了「发信失败绝不能影响业务」：
//
//  1. 投递发生在业务事务提交之后。调用点一律在 service 返回成功之后，不在事务
//     里 —— SMTP 握手可能耗时数秒，压在事务里会长时间持有行锁。
//  2. 投递是异步的。请求线程只把意图丢进队列就返回，用户不必等一封通知信。
//  3. 一切失败只记日志。没有邮件插件、插件报错、队列满，业务都照常成功。
//
// 队列不持久化：主程序重启会丢掉在途的通知信，这是可接受的代价 —— 为几封
// 通知信上一套 outbox 表并不划算。要保证送达时再加。
package notify

import (
	"context"
	"fmt"
	"sync"
	"time"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// queueSize 是待发队列的容量。
//
// 满了就丢弃并记 warning，绝不阻塞业务线程 —— 队列积压说明插件已经跟不上，
// 那时让用户的请求跟着一起卡住是最坏的选择。
const queueSize = 128

// sendTimeout 是单封信的超时。
//
// 比插件的能力调用超时宽一些：通知信在后台发，慢一点无所谓，但不能没有上限，
// 否则一个卡住的插件会把整个队列堵死。
const sendTimeout = 30 * time.Second

// Mailer 是发信能力的提供方，由 plugin.Manager 实现。
//
// 定义成接口而不是直接依赖 plugin 包：本包不关心信是怎么发出去的，测试里也
// 好塞一个假的进来。
type Mailer interface {
	SendMail(ctx context.Context, req *pb.SendMailRequest) error
}

// Recipient 是一位收件人。
type Recipient struct {
	Name  string
	Email string
}

// Resolver 提供发信要用到的、存在数据库里的信息。
//
// 由接口层实现：本包不该知道数据库的存在，也避免 notify → service 的依赖。
type Resolver interface {
	// Recipient 按用户 ID 取收件人。用户不存在时返回错误。
	Recipient(userID uint) (Recipient, error)
	// SiteName 返回站点名称，用于信件标题。
	SiteName() string
}

// mail 是队列里的一封待发信件。
type mail struct {
	userID  uint
	subject string
	body    string
}

// Notifier 是异步通知投递器。
//
// 零值不可用，必须经 New 构造。nil 的 *Notifier 上调用任何方法都是安全的空操作
// —— 没有插件系统时（测试、或插件未启用）调用点不必到处判空。
type Notifier struct {
	mailer   Mailer
	resolver Resolver
	logf     func(string, ...any)

	queue chan mail
	// start 保证 worker 只在第一次投递时才起来：绝大多数站点没装邮件插件，
	// 不该为此常驻一个 goroutine。
	start sync.Once
	// closed 由 Close 关闭，worker 据此退出。
	closed   chan struct{}
	closeOne sync.Once
	wg       sync.WaitGroup
}

// New 构造 Notifier。mailer 或 resolver 为 nil 时返回 nil（即全程空操作）。
func New(mailer Mailer, resolver Resolver, logf func(string, ...any)) *Notifier {
	if mailer == nil || resolver == nil {
		return nil
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Notifier{
		mailer:   mailer,
		resolver: resolver,
		logf:     logf,
		queue:    make(chan mail, queueSize),
		closed:   make(chan struct{}),
	}
}

// Close 停止 worker 并等它退出。主程序退出时调用。
//
// 队列里剩下的信直接丢掉：正在关机，没有时间也没有必要把它们发完。
func (n *Notifier) Close() {
	if n == nil {
		return
	}
	n.closeOne.Do(func() { close(n.closed) })
	n.wg.Wait()
}

// enqueue 把一封信放进队列，队列满时丢弃。
func (n *Notifier) enqueue(userID uint, subject, body string) {
	if n == nil || userID == 0 || subject == "" {
		return
	}
	n.start.Do(func() {
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			n.work()
		}()
	})

	select {
	case n.queue <- mail{userID: userID, subject: subject, body: body}:
	default:
		// 丢弃而不是阻塞：业务线程正在等着返回响应。
		n.logf("通知队列已满，丢弃一封邮件通知（主题：%s）", subject)
	case <-n.closed:
	}
}

// work 是单 worker 循环：一封一封地发，不并发。
//
// 单 worker 是刻意的：通知信没有并发发送的必要，而串行意味着插件那边只需要
// 应付一个调用者，也天然给出了一个上限。
func (n *Notifier) work() {
	for {
		select {
		case <-n.closed:
			return
		case item := <-n.queue:
			n.send(item)
		}
	}
}

// send 发一封信，任何失败只记日志。
func (n *Notifier) send(item mail) {
	to, err := n.resolver.Recipient(item.userID)
	if err != nil {
		n.logf("发送通知失败：查不到用户 %d 的邮箱：%v", item.userID, err)
		return
	}
	if to.Email == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()

	req := &pb.SendMailRequest{
		To:       []*pb.Mailbox{{Address: to.Email, Name: to.Name}},
		Subject:  item.subject,
		TextBody: item.body,
	}
	if err := n.mailer.SendMail(ctx, req); err != nil {
		// 这里是整个设计的落点：发信失败就只是日志里的一行，业务早已成功。
		n.logf("发送通知邮件失败（收件人 %s，主题 %s）：%v", to.Email, item.subject, err)
		return
	}
}

// subject 给标题加上站点名前缀。
func (n *Notifier) subject(format string, args ...any) string {
	return "[" + n.resolver.SiteName() + "] " + fmt.Sprintf(format, args...)
}
