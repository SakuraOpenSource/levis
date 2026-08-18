package notify

import (
	"fmt"
	"strings"
)

// 本文件是全部通知事件的清单。每个方法对应一个站内事件，正文在这里成文，
// 调用点只管传业务参数。
//
// 正文用纯文本，不拼 HTML：通知信的内容就是几行事实加一个链接，HTML 只会带来
// 转义问题。契约里的 html_body 留空，插件应当只发纯文本。
//
// 这些文案不进 i18n：它们由后端在没有请求上下文的 worker 里生成，拿不到用户的
// 语言偏好。项目当前只有 zh-CN 一种前端文案，等真要做多语言邮件时，得先有
// 「用户的语言」这个字段，那是另一件事。

// TicketReplied 通知用户：客服回复了工单。
//
// 只在客服回复时调用 —— 用户自己回复不必再收一封信。
func (n *Notifier) TicketReplied(userID uint, ticketNo, subject, body string) {
	if n == nil {
		return
	}
	n.enqueue(userID, n.subject("工单 %s 有新回复", ticketNo), strings.Join([]string{
		fmt.Sprintf("您的工单「%s」收到了新的回复：", subject),
		"",
		excerpt(body),
		"",
		"请登录站点查看完整内容并继续跟进。",
	}, "\n"))
}

// KYCReviewed 通知用户：实名审核有结果。
func (n *Notifier) KYCReviewed(userID uint, approved bool, reason string) {
	if n == nil {
		return
	}
	if approved {
		n.enqueue(userID, n.subject("实名认证已通过"), strings.Join([]string{
			"您的实名认证申请已通过审核。",
			"",
			"现在可以使用需要实名的功能了。",
		}, "\n"))
		return
	}
	n.enqueue(userID, n.subject("实名认证未通过"), strings.Join([]string{
		"您的实名认证申请未通过审核。",
		"",
		"原因：" + reason,
		"",
		"请核对信息后重新提交。",
	}, "\n"))
}

// OrderPaid 通知用户：订单支付成功。
func (n *Notifier) OrderPaid(userID uint, orderNo string, totalCents int64) {
	if n == nil {
		return
	}
	n.enqueue(userID, n.subject("订单 %s 已支付", orderNo), strings.Join([]string{
		fmt.Sprintf("订单 %s 已支付成功，金额 %s。", orderNo, yuan(totalCents)),
		"",
		"相关服务已开通，请登录站点查看。",
	}, "\n"))
}

// excerptLen 是正文摘录的字符数上限。
//
// 通知信只是「有新回复，去看看」，把整段正文塞进邮件既没必要，也把内容抄到了
// 一个安全性更弱的地方。
const excerptLen = 200

// excerpt 截取正文的前若干字符。
func excerpt(body string) string {
	body = strings.TrimSpace(body)
	runes := []rune(body)
	if len(runes) <= excerptLen {
		return body
	}
	return string(runes[:excerptLen]) + "……"
}

// yuan 把整数分格式化成「¥12.34」。
//
// 全系统的金额都是 int64 分，只在展示时换算，且用整数除余而不是浮点 ——
// 浮点在这里会给出 12.339999999999999 这种结果。
func yuan(cents int64) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s¥%d.%02d", sign, cents/100, cents%100)
}
