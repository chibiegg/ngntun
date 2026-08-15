package sip

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RFC 3261 のタイマ。UDP 上では再送の面倒を自分で見る必要がある。
const (
	timerT1 = 500 * time.Millisecond // 推定 RTT。再送間隔の初期値
	timerT2 = 4 * time.Second        // 非 INVITE の再送間隔の上限
	timerB  = 32 * time.Second       // INVITE トランザクションのタイムアウト (64*T1)
	timerF  = 32 * time.Second       // 非 INVITE トランザクションのタイムアウト (64*T1)
)

// clientTx は 1 本のクライアントトランザクション。
// 応答が来るまでリクエストを再送し、最終応答を呼び出し元へ渡す。
type clientTx struct {
	key    string
	req    *Message
	dst    netip.AddrPort
	tr     *Transport
	invite bool
	ua     *UA

	provisional chan *Message
	final       chan *Message
	errc        chan error

	gotResponse atomic.Bool
	stopOnce    sync.Once
	stop        chan struct{}

	// onProvisional は暫定応答が届くたびに呼ばれる (100rel の PRACK 送出などに使う)。
	onProvisional func(*Message)
}

func txKey(branch, method string) string {
	return branch + "|" + strings.ToUpper(method)
}

// begin はリクエストを送信し、再送ループを開始する。
func (u *UA) begin(tr *Transport, req *Message, dst netip.AddrPort) (*clientTx, error) {
	_, method := req.CSeq()
	tx := &clientTx{
		key:         txKey(req.Branch(), method),
		req:         req,
		dst:         dst,
		tr:          tr,
		invite:      method == MethodInvite,
		ua:          u,
		provisional: make(chan *Message, 8),
		final:       make(chan *Message, 1),
		errc:        make(chan error, 1),
		stop:        make(chan struct{}),
	}

	u.mu.Lock()
	u.txs[tx.key] = tx
	u.mu.Unlock()

	if err := tr.Send(req, dst); err != nil {
		tx.close()
		return nil, err
	}
	go tx.retransmitLoop()
	return tx, nil
}

// retransmitLoop は最終応答が来るかタイムアウトするまでリクエストを再送する。
func (tx *clientTx) retransmitLoop() {
	timeout := timerF
	if tx.invite {
		timeout = timerB
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	interval := timerT1
	for {
		retry := time.NewTimer(interval)
		select {
		case <-tx.stop:
			retry.Stop()
			return

		case <-deadline.C:
			retry.Stop()
			tx.fail(fmt.Errorf("%s の応答がありません (%s 経過)", tx.req.Method, timeout))
			return

		case <-retry.C:
			// INVITE は暫定応答を受けた時点で再送を止める (RFC 3261 17.1.1)。
			// 非 INVITE は最終応答が来るまで T2 間隔で再送を続ける。
			if tx.invite && tx.gotResponse.Load() {
				return
			}
			if err := tx.tr.Send(tx.req, tx.dst); err != nil {
				tx.fail(err)
				return
			}
			interval *= 2
			if !tx.invite && interval > timerT2 {
				interval = timerT2
			}
		}
	}
}

// receive は UA の受信ループから呼ばれ、応答をトランザクションへ渡す。
func (tx *clientTx) receive(m *Message) {
	tx.gotResponse.Store(true)
	if m.IsProvisional() {
		select {
		case tx.provisional <- m:
		default: // 暫定応答が溜まっても捨てて構わない
		}
		return
	}
	select {
	case tx.final <- m:
	default: // 最終応答の再送。1 通目だけを採用する
	}
}

func (tx *clientTx) fail(err error) {
	select {
	case tx.errc <- err:
	default:
	}
}

// wait は最終応答を待つ。暫定応答は onProvisional に流す。
func (tx *clientTx) wait(ctx context.Context) (*Message, error) {
	for {
		select {
		case m := <-tx.final:
			return m, nil
		case err := <-tx.errc:
			return nil, err
		case m := <-tx.provisional:
			if tx.onProvisional != nil {
				tx.onProvisional(m)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// close は再送を止め、トランザクションを UA から取り除く。
func (tx *clientTx) close() {
	tx.stopOnce.Do(func() {
		close(tx.stop)
		tx.ua.mu.Lock()
		delete(tx.ua.txs, tx.key)
		tx.ua.mu.Unlock()
	})
}
