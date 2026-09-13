// sip-push：基于 RFC 8599 思路的未注册分机来电推送服务
// （AMI + DialBegin + PJSIPShowAors/IAXpeerlist + 多渠道推送，支持 PJSIP/IAX2 分机）
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"sip-push/internal/ami"
	"sip-push/internal/bark"
	"sip-push/internal/config"
	"sip-push/internal/monitor"
	"sip-push/internal/notify"
	"sip-push/internal/yakphone"
)

func main() {
	configPath := flag.String("c", "config.yaml", "配置文件路径")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatalf("加载配置失败: %v", err)
	}

	// 装配全部已配置的推送渠道（bark / yakphone，可同时启用）
	var pushers []notify.Pusher
	if cfg.Bark.DeviceKey != "" {
		c := bark.New(bark.Config{
			BaseURL:     cfg.Bark.BaseURL,
			DeviceKey:   cfg.Bark.DeviceKey,
			Group:       cfg.Bark.Group,
			PushTimeout: cfg.Bark.PushTimeout.Std(),
			Extensions:  cfg.Bark.Extensions,
		}, logger)
		pushers = append(pushers, c)
		logger.Printf("推送渠道已启用: %s（绑定分机: %s）", c.Name(), c.Bindings())
	}
	if cfg.Yakphone.Token != "" {
		c := yakphone.New(yakphone.Config{
			BaseURL:     cfg.Yakphone.BaseURL,
			Token:       cfg.Yakphone.Token,
			Domain:      cfg.Yakphone.Domain,
			PushTimeout: cfg.Yakphone.PushTimeout.Std(),
			Extensions:  cfg.Yakphone.Extensions,
		}, logger)
		pushers = append(pushers, c)
		logger.Printf("推送渠道已启用: %s（绑定分机: %s）", c.Name(), c.Bindings())
	}

	// monitor 需要在创建 ami.Client 时就作为事件回调，先建 monitor 再建 client
	mon, err := monitor.New(nil, pushers, monitor.Config{
		Technologies: cfg.Call.Technologies,
		ExtPattern:   cfg.Call.ExtPattern,
		DedupWindow:  cfg.Call.DedupWindow.Std(),
	}, logger)
	if err != nil {
		logger.Fatalf("初始化监控器失败: %v", err)
	}

	client := ami.New(ami.Config{
		Addr:              cfg.AMI.Addr,
		Username:          cfg.AMI.Username,
		Secret:            cfg.AMI.Secret,
		ReconnectInterval: cfg.AMI.ReconnectInterval.Std(),
		PingInterval:      cfg.AMI.PingInterval.Std(),
		DialTimeout:       cfg.AMI.DialTimeout.Std(),
		ActionTimeout:     cfg.AMI.ActionTimeout.Std(),
	}, logger, mon.OnEvent)
	mon.BindAMI(client) // 绑定查询接口，避免构造期循环依赖

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go client.Run(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Printf("收到信号 %v，退出", sig)
	cancel()
}
