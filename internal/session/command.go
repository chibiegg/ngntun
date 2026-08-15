package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// commandKillDelay は終了要求 (SIGTERM) からの猶予。過ぎたら SIGKILL する。
// 呼が生きている間は課金され続けるので、居座るコマンドを待ち続けない。
const commandKillDelay = 5 * time.Second

// commandResult は実行したコマンドの結果。
type commandResult struct {
	exitCode int
	err      error // 起動後の待ち受けそのものが失敗した場合のみ (終了コード非 0 はエラーではない)
}

// commandRunner はトンネル確立後に実行するコマンドの実行状態。
//
// 呼とコマンドの寿命を一致させるのが役目で、コマンドが終われば呼を切り、
// 呼が先に終わればコマンドを終了させる。
type commandRunner struct {
	cmd    *exec.Cmd
	exited chan struct{} // プロセス終了で閉じる
	res    commandResult // exited が閉じたあとだけ読んでよい
}

// startCommand はコマンドを起動する。
// ctx がキャンセルされたら SIGTERM を送り、commandKillDelay を過ぎても終わらなければ SIGKILL する。
func startCommand(ctx context.Context, cfg Config, r *Result, log *slog.Logger) (*commandRunner, error) {
	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), commandEnv(cfg, r)...)
	// 既定の Cancel は SIGKILL なので、後始末の機会を与えるため SIGTERM に差し替える。
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = commandKillDelay

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("コマンド %q を実行できません: %w", cfg.Command[0], err)
	}
	log.Info("コマンドを実行します", "cmd", cfg.Command, "pid", cmd.Process.Pid)

	run := &commandRunner{cmd: cmd, exited: make(chan struct{})}
	go func() {
		run.res = commandResultOf(cmd.Wait())
		close(run.exited)
	}()
	return run, nil
}

// Exited はコマンドの終了で閉じるチャネルを返す。
// コマンド未指定 (nil レシーバ) では nil を返すので、select では単に選ばれない。
func (r *commandRunner) Exited() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.exited
}

// Wait はコマンドの終了を待って結果を返す。
// WaitDelay があるため、ctx がキャンセルされていれば有限時間で返る。
func (r *commandRunner) Wait() commandResult {
	<-r.exited
	return r.res
}

// commandResultOf は exec.Cmd.Wait のエラーを終了コードに直す。
func commandResultOf(err error) commandResult {
	if err == nil {
		return commandResult{}
	}

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return commandResult{exitCode: 1, err: err}
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		// シェルと同じ 128+シグナル番号にする。こちらが SIGTERM で止めた場合もここに来る。
		return commandResult{exitCode: 128 + int(ws.Signal())}
	}
	return commandResult{exitCode: ee.ExitCode()}
}

// commandEnv はコマンドに渡す環境変数を組み立てる。
// トンネルの宛先やデバイス名をスクリプト側から参照できるようにするため。
func commandEnv(cfg Config, r *Result) []string {
	env := []string{
		"NGNTUN_TUN=" + cfg.TunName,
		"NGNTUN_TUN_MTU=" + strconv.Itoa(cfg.TunMTU),
	}
	add := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	if cfg.TunAddr.IsValid() {
		add("NGNTUN_TUN_ADDR", cfg.TunAddr.String())
		add("NGNTUN_TUN_IP", cfg.TunAddr.Addr().String())
	}
	if cfg.TunPeer.IsValid() {
		add("NGNTUN_TUN_PEER", cfg.TunPeer.String())
	}
	add("NGNTUN_PEER_NUMBER", r.PeerNumber)
	if r.Remote.IsValid() {
		add("NGNTUN_REMOTE_MEDIA", r.Remote.String())
	}
	return env
}
