package session

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestStartCommandExitCode(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want int
	}{
		{"正常終了", []string{"sh", "-c", "exit 0"}, 0},
		{"異常終了", []string{"sh", "-c", "exit 7"}, 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, err := startCommand(context.Background(), Config{Command: tt.argv}, &Result{}, testLogger())
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			select {
			case <-run.Exited():
			case <-time.After(10 * time.Second):
				t.Fatal("コマンドが終了しません")
			}
			if got := run.Wait(); got.exitCode != tt.want || got.err != nil {
				t.Errorf("exitCode=%d err=%v, want %d", got.exitCode, got.err, tt.want)
			}
		})
	}
}

// セッションが先に終わった場合、コマンドには SIGTERM が飛ぶ。
func TestStartCommandTerminatedOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	run, err := startCommand(ctx, Config{Command: []string{"sleep", "60"}}, &Result{}, testLogger())
	if err != nil {
		t.Fatalf("startCommand: %v", err)
	}

	select {
	case <-run.Exited():
		t.Fatal("キャンセル前に終了しました")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	done := make(chan commandResult, 1)
	go func() { done <- run.Wait() }()

	select {
	case res := <-done:
		if res.exitCode != 128+15 { // SIGTERM
			t.Errorf("exitCode=%d, want %d (SIGTERM)", res.exitCode, 128+15)
		}
	case <-time.After(commandKillDelay + 5*time.Second):
		t.Fatal("キャンセルしてもコマンドが終了しません")
	}
}

func TestStartCommandNotFound(t *testing.T) {
	_, err := startCommand(context.Background(),
		Config{Command: []string{"ngntun-no-such-command"}}, &Result{}, testLogger())
	if err == nil {
		t.Fatal("存在しないコマンドでエラーになりません")
	}
}

func TestCommandEnv(t *testing.T) {
	cfg := Config{
		TunName: "dc0",
		TunMTU:  1452,
		TunAddr: netip.MustParsePrefix("172.21.0.100/30"),
		TunPeer: netip.MustParseAddr("172.21.0.101"),
	}
	res := &Result{PeerNumber: "0312345678", Remote: netip.MustParseAddrPort("[2001:db8::1]:5004")}

	env := commandEnv(cfg, res)
	want := []string{
		"NGNTUN_TUN=dc0",
		"NGNTUN_TUN_MTU=1452",
		"NGNTUN_TUN_ADDR=172.21.0.100/30",
		"NGNTUN_TUN_IP=172.21.0.100",
		"NGNTUN_TUN_PEER=172.21.0.101",
		"NGNTUN_PEER_NUMBER=0312345678",
		"NGNTUN_REMOTE_MEDIA=[2001:db8::1]:5004",
	}
	for _, w := range want {
		if !slices.Contains(env, w) {
			t.Errorf("環境変数 %q がありません: %v", w, env)
		}
	}

	// 未設定の値は渡さない (空文字の変数を作らない)。
	for _, e := range commandEnv(Config{TunName: "dc0"}, &Result{}) {
		if len(e) > 0 && e[len(e)-1] == '=' {
			t.Errorf("空の環境変数を渡しています: %q", e)
		}
	}
}
