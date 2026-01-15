package utils

import (
	"crypto/tls"
	"fmt"
	"net/smtp"
)

// SMTPConfig holds SMTP connection settings.
type SMTPConfig struct {
	Host string
	Port int
	User string
	Pass string
	To   string
}

// SendEmail 使用 SMTP 发送纯文本电子邮件
func SendEmail(cfg SMTPConfig, subject, body string) error {
	if cfg.Host == "" || cfg.Port == 0 || cfg.User == "" || cfg.Pass == "" || cfg.To == "" {
		return fmt.Errorf("smtp config is incomplete")
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	// QQ SMTP 要求包含 From/To 头，使用纯文本 MIME。
	msg := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s", cfg.User, cfg.To, subject, body))

	if cfg.Port == 465 {
		// 465 端口使用 TLS 直连
		tlsConfig := &tls.Config{ServerName: cfg.Host}
		conn, err := tls.Dial("tcp", addr, tlsConfig)
		if err != nil {
			return fmt.Errorf("smtp tls dial: %w", err)
		}
		client, err := smtp.NewClient(conn, cfg.Host)
		if err != nil {
			return fmt.Errorf("smtp client: %w", err)
		}
		defer client.Close()

		auth := smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
		// 按 SMTP 流程设置发件人与收件人
		if err := client.Mail(cfg.User); err != nil {
			return fmt.Errorf("smtp mail: %w", err)
		}
		if err := client.Rcpt(cfg.To); err != nil {
			return fmt.Errorf("smtp rcpt: %w", err)
		}
		// 写入邮件正文
		writer, err := client.Data()
		if err != nil {
			return fmt.Errorf("smtp data: %w", err)
		}
		if _, err := writer.Write(msg); err != nil {
			_ = writer.Close()
			return fmt.Errorf("smtp write: %w", err)
		}
		if err := writer.Close(); err != nil {
			return fmt.Errorf("smtp close: %w", err)
		}
		if err := client.Quit(); err != nil {
			// 部分 SMTP 服务器在 DATA 后会主动断开连接，忽略 QUIT 错误
			return nil
		}
		return nil
	}

	// 其它端口使用标准 SMTP 发送
	auth := smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)
	if err := smtp.SendMail(addr, auth, cfg.User, []string{cfg.To}, msg); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}
