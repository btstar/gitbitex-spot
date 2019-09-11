// Copyright 2019 GitBitEx.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pushing

import (
	"context"
	"encoding/json"
	"github.com/gitbitex/gitbitex-spot/service"
	"github.com/gorilla/websocket"
	"github.com/siddontang/go-log/log"
	"sync"
	"sync/atomic"
	"time"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 512
)

var id int64

// 每个连接对应一个client，client负责该连接的数据I/O
type Client struct {
	id         int64
	conn       *websocket.Conn
	writeCh    chan interface{}
	l2ChangeCh chan *Level2Change
	sub        *subscription
	channels   map[string]struct{}
	mu         sync.Mutex
}

func NewClient(conn *websocket.Conn, sub *subscription) *Client {
	return &Client{
		id:         atomic.AddInt64(&id, 1),
		conn:       conn,
		writeCh:    make(chan interface{}, 256),
		l2ChangeCh: make(chan *Level2Change, 512),
		sub:        sub,
		channels:   map[string]struct{}{},
	}
}

func (c *Client) startServe() {
	go c.runReader()
	go c.runWriter()
}

func (c *Client) runReader() {
	c.conn.SetReadLimit(maxMessageSize)
	err := c.conn.SetReadDeadline(time.Now().Add(pongWait))
	if err != nil {
		log.Error(err)
	}
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			c.close()
			break
		}

		var req Request
		err = json.Unmarshal(message, &req)
		if err != nil {
			log.Errorf("bad message : %v %v", string(message), err)
			c.close()
			break
		}

		c.onMessage(&req)
	}
}

func (c *Client) runWriter() {
	ctx, cancel := context.WithCancel(context.Background())
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		cancel()
		ticker.Stop()
		_ = c.conn.Close()
	}()

	go c.runL2ChangeWriter(ctx)

	for {
		select {
		case message := <-c.writeCh:
			// 转发l2change消息，进行增量推送
			switch message.(type) {
			case *Level2Change:
				c.l2ChangeCh <- message.(*Level2Change)
				continue
			}

			err := c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err != nil {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				c.close()
				return
			}

			buf, err := json.Marshal(message)
			if err != nil {
				continue
			}
			err = c.conn.WriteMessage(websocket.TextMessage, buf)
			if err != nil {
				c.close()
				return
			}

		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			err := c.conn.WriteMessage(websocket.PingMessage, nil)
			if err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *Client) runL2ChangeWriter(ctx context.Context) {
	var resendSnapshot = true
	var changes []*Level2Change
	var lastSeq int64 = 0

	for {
		select {
		case <-ctx.Done():
			return
		case l2Change := <-c.l2ChangeCh:
			if resendSnapshot || l2Change.Seq == 0 {
				snapshot, err := sharedSnapshotStore().getLastLevel2(l2Change.ProductId)
				if err != nil {
					log.Error(err)
					continue
				}

				// 最新的snapshot版本太旧了，丢弃，等待更新的snapshot版本
				if lastSeq > snapshot.Seq {
					log.Warnf("last snapshot too old: %v changeSeq=%v snapshotSeq=%v",
						l2Change.ProductId, lastSeq, snapshot.Seq)
					continue
				}

				lastSeq = snapshot.Seq
				resendSnapshot = false

				c.writeCh <- &Level2Message{
					Type:      Level2TypeSnapshot,
					ProductId: l2Change.ProductId,
					Bids:      snapshot.Bids,
					Asks:      snapshot.Asks,
					Version:   snapshot.Seq,
				}
				continue
			}

			// 丢弃seq小于snapshot seq的变更
			if l2Change.Seq <= lastSeq {
				log.Infof("discard l2changeSeq=%v snapshotSeq=%v", l2Change.Seq, lastSeq)
				continue
			}

			// seq不连续，发生了消息丢失，重新发送快照
			if l2Change.Seq != lastSeq+1 {
				log.Infof("l2change lost newSeq=%v lastSeq=%v", l2Change.Seq, lastSeq)
				resendSnapshot = true
				changes = nil
				lastSeq = l2Change.Seq
				if len(c.l2ChangeCh) == 0 {
					c.l2ChangeCh <- &Level2Change{ProductId: l2Change.ProductId}
				}
				continue
			}

			lastSeq = l2Change.Seq
			changes = append(changes, l2Change)

			// 如果chan还有消息继续读满缓冲区
			if len(c.l2ChangeCh) > 0 && len(changes) < 10 {
				continue
			}

			updateMsg := &Level2Message{
				Type:      Level2TypeUpdate,
				ProductId: l2Change.ProductId,
				Asks:      [][3]interface{}{},
				Bids:      [][3]interface{}{},
			}
			for _, change := range changes {
				updateMsg.Changes = append(updateMsg.Changes, [3]interface{}{change.Side, change.Price, change.Size})
			}
			c.writeCh <- updateMsg
			changes = nil
		}
	}
}

func (c *Client) onMessage(req *Request) {
	switch req.Type {
	case "subscribe":
		c.onSub(req.CurrencyIds, req.ProductIds, req.Channels, req.Token)
	default:
	}
}

func (c *Client) onSub(currencyIds []string, productIds []string, channels []string, token string) {
	user, err := service.CheckToken(token)
	if err != nil {
		log.Error(err)
	}

	var userId int64
	if user != nil {
		userId = user.Id
	}

	for range currencyIds {
		for _, channel := range channels {
			switch Channel(channel) {
			case ChannelFunds:
				c.subscribe(ChannelFunds.FormatWithUserId(userId), c)
			}
		}
	}

	for _, productId := range productIds {
		for _, channel := range channels {
			switch Channel(channel) {
			case ChannelLevel2:
				log.Info(c.id)
				if c.subscribe(ChannelLevel2.FormatWithProductId(productId), c) {
					if len(c.l2ChangeCh) == 0 {
						c.l2ChangeCh <- &Level2Change{ProductId: productId}
					}
				}

			case ChannelMatch:
				c.subscribe(ChannelMatch.FormatWithProductId(productId), c)

			case ChannelTicker:
				c.subscribe(ChannelTicker.FormatWithProductId(productId), c)

			case ChannelOrder:
				c.subscribe(ChannelOrder.Format(productId, userId), c)

			default:
				continue
			}
		}
	}
}

func (c *Client) subscribe(channel string, client *Client) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, found := c.channels[channel]
	if found {
		return false
	}

	if c.sub.subscribe(channel, client) {
		c.channels[channel] = struct{}{}
		return true
	}
	return false
}

func (c *Client) close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for channel, _ := range c.channels {
		c.sub.unsubscribe(channel, c)
	}
}