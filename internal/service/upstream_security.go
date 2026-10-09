package service

import (
	"log"
	"net"
	"net/url"
	"strings"

	"github.com/SakuraOpenSource/levis/internal/model"
)

func upstreamHTTPAllowed(config map[string]string) error {
	flag := config["allow_insecure"]
	if flag != "" && flag != "true" && flag != "false" {
		return ErrBadRequest("allow_insecure 必须明确设置为 true 或 false")
	}
	address := strings.TrimSpace(config["api_url"])
	if address == "" {
		return nil
	}
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return ErrBadRequest("api_url 必须是无内嵌凭据的 HTTP 或 HTTPS 地址")
	}
	if u.Scheme == "http" && !upstreamLoopback(u.Hostname()) && flag != "true" {
		return ErrBadRequest("非回环 HTTP 会暴露接口凭据；请使用 HTTPS 或明确设置 allow_insecure=true")
	}
	return nil
}

func upstreamLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func legacyUpstreamConfig(config model.OptionMap, interfaceID uint) model.OptionMap {
	out := make(model.OptionMap, len(config)+1)
	for key, value := range config {
		out[key] = value
	}
	if _, explicit := out["allow_insecure"]; !explicit {
		u, err := url.Parse(out["api_url"])
		if err == nil && u.Scheme == "http" && !upstreamLoopback(u.Hostname()) {
			// Preserve existing deployments, but make the insecure exception visible without logging credentials.
			out["allow_insecure"] = "true"
			log.Printf("WARNING interface=%d uses legacy non-loopback HTTP; migrate to HTTPS", interfaceID)
		}
	}
	return out
}
