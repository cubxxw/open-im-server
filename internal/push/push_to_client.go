// Copyright © 2023 OpenIM. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package push

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/openimsdk/open-im-server/v3/pkg/util/conversationutil"
	"github.com/openimsdk/tools/utils/datautil"
	"github.com/openimsdk/tools/utils/jsonutil"
	"github.com/openimsdk/tools/utils/stringutil"
	"sync"

	"github.com/openimsdk/open-im-server/v3/internal/push/offlinepush"
	"github.com/openimsdk/open-im-server/v3/internal/push/offlinepush/dummy"
	"github.com/openimsdk/open-im-server/v3/internal/push/offlinepush/fcm"
	"github.com/openimsdk/open-im-server/v3/internal/push/offlinepush/getui"
	"github.com/openimsdk/open-im-server/v3/internal/push/offlinepush/jpush"
	"github.com/openimsdk/open-im-server/v3/pkg/common/config"
	"github.com/openimsdk/open-im-server/v3/pkg/common/db/cache"
	"github.com/openimsdk/open-im-server/v3/pkg/common/db/controller"
	"github.com/openimsdk/open-im-server/v3/pkg/common/prommetrics"
	"github.com/openimsdk/open-im-server/v3/pkg/msgprocessor"
	"github.com/openimsdk/open-im-server/v3/pkg/rpccache"
	"github.com/openimsdk/open-im-server/v3/pkg/rpcclient"
	"github.com/openimsdk/protocol/constant"
	"github.com/openimsdk/protocol/conversation"
	"github.com/openimsdk/protocol/msggateway"
	"github.com/openimsdk/protocol/sdkws"
	"github.com/openimsdk/tools/discovery"
	"github.com/openimsdk/tools/log"
	"github.com/openimsdk/tools/mcontext"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

type Pusher struct {
	config                 *config.GlobalConfig
	database               controller.PushDatabase
	discov                 discovery.SvcDiscoveryRegistry
	offlinePusher          offlinepush.OfflinePusher
	groupLocalCache        *rpccache.GroupLocalCache
	conversationLocalCache *rpccache.ConversationLocalCache
	msgRpcClient           *rpcclient.MessageRpcClient
	conversationRpcClient  *rpcclient.ConversationRpcClient
	groupRpcClient         *rpcclient.GroupRpcClient
}

var errNoOfflinePusher = errors.New("no offlinePusher is configured")

func NewPusher(config *config.GlobalConfig, discov discovery.SvcDiscoveryRegistry, offlinePusher offlinepush.OfflinePusher, database controller.PushDatabase,
	groupLocalCache *rpccache.GroupLocalCache, conversationLocalCache *rpccache.ConversationLocalCache,
	conversationRpcClient *rpcclient.ConversationRpcClient, groupRpcClient *rpcclient.GroupRpcClient, msgRpcClient *rpcclient.MessageRpcClient,
) *Pusher {
	return &Pusher{
		config:                 config,
		discov:                 discov,
		database:               database,
		offlinePusher:          offlinePusher,
		groupLocalCache:        groupLocalCache,
		conversationLocalCache: conversationLocalCache,
		msgRpcClient:           msgRpcClient,
		conversationRpcClient:  conversationRpcClient,
		groupRpcClient:         groupRpcClient,
	}
}

func NewOfflinePusher(pushConf *config.Push, iOSPushConf *config.IOSPush, cache cache.MsgModel) (offlinepush.OfflinePusher, error) {
	var offlinePusher offlinepush.OfflinePusher
	switch pushConf.Enable {
	case "getui":
		offlinePusher = getui.NewClient(pushConf, cache)
	case "fcm":
		return fcm.NewClient(pushConf, cache)
	case "jpush":
		offlinePusher = jpush.NewClient(pushConf, iOSPushConf)
	default:
		offlinePusher = dummy.NewClient()
	}
	return offlinePusher, nil
}

func (p *Pusher) DeleteMemberAndSetConversationSeq(ctx context.Context, groupID string, userIDs []string) error {
	conevrsationID := msgprocessor.GetConversationIDBySessionType(constant.SuperGroupChatType, groupID)
	maxSeq, err := p.msgRpcClient.GetConversationMaxSeq(ctx, conevrsationID)
	if err != nil {
		return err
	}
	return p.conversationRpcClient.SetConversationMaxSeq(ctx, userIDs, conevrsationID, maxSeq)
}

func (p *Pusher) Push2User(ctx context.Context, userIDs []string, msg *sdkws.MsgData) error {
	log.ZDebug(ctx, "Get msg from msg_transfer And push msg", "userIDs", userIDs, "msg", msg.String())
	if err := callbackOnlinePush(ctx, &p.config.Callback, userIDs, msg); err != nil {
		return err
	}
	// push
	wsResults, err := p.GetConnsAndOnlinePush(ctx, msg, userIDs)
	if err != nil {
		return err
	}

	isOfflinePush := datautil.GetSwitchFromOptions(msg.Options, constant.IsOfflinePush)
	log.ZDebug(ctx, "push_result", "ws push result", wsResults, "sendData", msg, "isOfflinePush", isOfflinePush, "push_to_userID", userIDs)

	if !isOfflinePush {
		return nil
	}

	if len(wsResults) == 0 {
		return nil
	}
	onlinePushSuccUserIDSet := datautil.SliceSet(datautil.Filter(wsResults, func(e *msggateway.SingleMsgToUserResults) (string, bool) {
		return e.UserID, e.OnlinePush && e.UserID != ""
	}))
	offlinePushUserIDList := datautil.Filter(wsResults, func(e *msggateway.SingleMsgToUserResults) (string, bool) {
		_, exist := onlinePushSuccUserIDSet[e.UserID]
		return e.UserID, !exist && e.UserID != "" && e.UserID != msg.SendID
	})

	if len(offlinePushUserIDList) > 0 {
		if err = callbackOfflinePush(ctx, &p.config.Callback, offlinePushUserIDList, msg, &[]string{}); err != nil {
			return err
		}
		err = p.offlinePushMsg(ctx, msg.SendID, msg, offlinePushUserIDList)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Pusher) UnmarshalNotificationElem(bytes []byte, t any) error {
	var notification sdkws.NotificationElem
	if err := json.Unmarshal(bytes, &notification); err != nil {
		return err
	}

	return json.Unmarshal([]byte(notification.Detail), t)
}

/*
k8s deployment,offline push group messages function.
*/
func (p *Pusher) k8sOfflinePush2SuperGroup(ctx context.Context, groupID string, msg *sdkws.MsgData, wsResults []*msggateway.SingleMsgToUserResults) error {

	var needOfflinePushUserIDs []string
	for _, v := range wsResults {
		if !v.OnlinePush {
			needOfflinePushUserIDs = append(needOfflinePushUserIDs, v.UserID)
		}
	}
	if len(needOfflinePushUserIDs) > 0 {
		var offlinePushUserIDs []string
		err := callbackOfflinePush(ctx, &p.config.Callback, needOfflinePushUserIDs, msg, &offlinePushUserIDs)
		if err != nil {
			return err
		}

		if len(offlinePushUserIDs) > 0 {
			needOfflinePushUserIDs = offlinePushUserIDs
		}
		if msg.ContentType != constant.SignalingNotification {
			resp, err := p.conversationRpcClient.Client.GetConversationOfflinePushUserIDs(
				ctx,
				&conversation.GetConversationOfflinePushUserIDsReq{ConversationID: conversationutil.GenGroupConversationID(groupID), UserIDs: needOfflinePushUserIDs},
			)
			if err != nil {
				return err
			}
			if len(resp.UserIDs) > 0 {
				err = p.offlinePushMsg(ctx, groupID, msg, resp.UserIDs)
				if err != nil {
					log.ZError(ctx, "offlinePushMsg failed", err, "groupID", groupID, "msg", msg)
					return err
				}
			}
		}

	}
	return nil
}
func (p *Pusher) Push2SuperGroup(ctx context.Context, groupID string, msg *sdkws.MsgData) (err error) {
	log.ZDebug(ctx, "Get super group msg from msg_transfer and push msg", "msg", msg.String(), "groupID", groupID)
	var pushToUserIDs []string
	if err = callbackBeforeSuperGroupOnlinePush(ctx, &p.config.Callback, groupID, msg, &pushToUserIDs); err != nil {
		return err
	}

	if len(pushToUserIDs) == 0 {
		pushToUserIDs, err = p.groupLocalCache.GetGroupMemberIDs(ctx, groupID)
		if err != nil {
			return err
		}

		switch msg.ContentType {
		case constant.MemberQuitNotification:
			var tips sdkws.MemberQuitTips
			if p.UnmarshalNotificationElem(msg.Content, &tips) != nil {
				return err
			}
			defer func(groupID string, userIDs []string) {
				if err = p.DeleteMemberAndSetConversationSeq(ctx, groupID, userIDs); err != nil {
					log.ZError(ctx, "MemberQuitNotification DeleteMemberAndSetConversationSeq", err, "groupID", groupID, "userIDs", userIDs)
				}
			}(groupID, []string{tips.QuitUser.UserID})
			pushToUserIDs = append(pushToUserIDs, tips.QuitUser.UserID)
		case constant.MemberKickedNotification:
			var tips sdkws.MemberKickedTips
			if p.UnmarshalNotificationElem(msg.Content, &tips) != nil {
				return err
			}
			kickedUsers := datautil.Slice(tips.KickedUserList, func(e *sdkws.GroupMemberFullInfo) string { return e.UserID })
			defer func(groupID string, userIDs []string) {
				if err = p.DeleteMemberAndSetConversationSeq(ctx, groupID, userIDs); err != nil {
					log.ZError(ctx, "MemberKickedNotification DeleteMemberAndSetConversationSeq", err, "groupID", groupID, "userIDs", userIDs)
				}
			}(groupID, kickedUsers)
			pushToUserIDs = append(pushToUserIDs, kickedUsers...)
		case constant.GroupDismissedNotification:
			// Messages arrive first, notifications arrive later
			if msgprocessor.IsNotification(msgprocessor.GetConversationIDByMsg(msg)) {
				var tips sdkws.GroupDismissedTips
				if p.UnmarshalNotificationElem(msg.Content, &tips) != nil {
					return err
				}
				log.ZInfo(ctx, "GroupDismissedNotificationInfo****", "groupID", groupID, "num", len(pushToUserIDs), "list", pushToUserIDs)
				if len(p.config.Manager.UserID) > 0 {
					ctx = mcontext.WithOpUserIDContext(ctx, p.config.Manager.UserID[0])
				}
				if len(p.config.Manager.UserID) == 0 && len(p.config.IMAdmin.UserID) > 0 {
					ctx = mcontext.WithOpUserIDContext(ctx, p.config.IMAdmin.UserID[0])
				}
				defer func(groupID string) {
					if err = p.groupRpcClient.DismissGroup(ctx, groupID); err != nil {
						log.ZError(ctx, "DismissGroup Notification clear members", err, "groupID", groupID)
					}
				}(groupID)
			}
		}
	}

	wsResults, err := p.GetConnsAndOnlinePush(ctx, msg, pushToUserIDs)
	if err != nil {
		return err
	}

	log.ZDebug(ctx, "get conn and online push success", "result", wsResults, "msg", msg)
	isOfflinePush := datautil.GetSwitchFromOptions(msg.Options, constant.IsOfflinePush)
	if isOfflinePush && p.config.Envs.Discovery == "k8s" {
		return p.k8sOfflinePush2SuperGroup(ctx, groupID, msg, wsResults)
	}
	if isOfflinePush && p.config.Envs.Discovery == "zookeeper" {
		var (
			onlineSuccessUserIDs      = []string{msg.SendID}
			webAndPcBackgroundUserIDs []string
		)

		for _, v := range wsResults {
			if v.OnlinePush && v.UserID != msg.SendID {
				onlineSuccessUserIDs = append(onlineSuccessUserIDs, v.UserID)
			}

			if v.OnlinePush {
				continue
			}

			if len(v.Resp) == 0 {
				continue
			}

			for _, singleResult := range v.Resp {
				if singleResult.ResultCode != -2 {
					continue
				}

				isPC := constant.PlatformIDToName(int(singleResult.RecvPlatFormID)) == constant.TerminalPC
				isWebID := singleResult.RecvPlatFormID == constant.WebPlatformID

				if isPC || isWebID {
					webAndPcBackgroundUserIDs = append(webAndPcBackgroundUserIDs, v.UserID)
				}
			}
		}

		needOfflinePushUserIDs := stringutil.DifferenceString(onlineSuccessUserIDs, pushToUserIDs)

		// Use offline push messaging
		if len(needOfflinePushUserIDs) > 0 {
			var offlinePushUserIDs []string
			err = callbackOfflinePush(ctx, &p.config.Callback, needOfflinePushUserIDs, msg, &offlinePushUserIDs)
			if err != nil {
				return err
			}

			if len(offlinePushUserIDs) > 0 {
				needOfflinePushUserIDs = offlinePushUserIDs
			}
			if msg.ContentType != constant.SignalingNotification {
				resp, err := p.conversationRpcClient.Client.GetConversationOfflinePushUserIDs(
					ctx,
					&conversation.GetConversationOfflinePushUserIDsReq{ConversationID: conversationutil.GenGroupConversationID(groupID), UserIDs: needOfflinePushUserIDs},
				)
				if err != nil {
					return err
				}
				if len(resp.UserIDs) > 0 {
					err = p.offlinePushMsg(ctx, groupID, msg, resp.UserIDs)
					if err != nil {
						log.ZError(ctx, "offlinePushMsg failed", err, "groupID", groupID, "msg", msg)
						return err
					}
					if _, err := p.GetConnsAndOnlinePush(ctx, msg, stringutil.IntersectString(resp.UserIDs, webAndPcBackgroundUserIDs)); err != nil {
						log.ZError(ctx, "offlinePushMsg failed", err, "groupID", groupID, "msg", msg, "userIDs", stringutil.IntersectString(needOfflinePushUserIDs, webAndPcBackgroundUserIDs))
						return err
					}
				}
			}

		}
	}
	return nil
}

func (p *Pusher) k8sOnlinePush(ctx context.Context, msg *sdkws.MsgData, pushToUserIDs []string) (wsResults []*msggateway.SingleMsgToUserResults, err error) {
	var usersHost = make(map[string][]string)
	for _, v := range pushToUserIDs {
		tHost, err := p.discov.GetUserIdHashGatewayHost(ctx, v)
		if err != nil {
			log.ZError(ctx, "get msggateway hash error", err)
			return nil, err
		}
		tUsers, tbl := usersHost[tHost]
		if tbl {
			tUsers = append(tUsers, v)
			usersHost[tHost] = tUsers
		} else {
			usersHost[tHost] = []string{v}
		}
	}
	log.ZDebug(ctx, "genUsers send hosts struct:", "usersHost", usersHost)
	var usersConns = make(map[*grpc.ClientConn][]string)
	for host, userIds := range usersHost {
		tconn, _ := p.discov.GetConn(ctx, host)
		usersConns[tconn] = userIds
	}
	var (
		mu         sync.Mutex
		wg         = errgroup.Group{}
		maxWorkers = p.config.Push.MaxConcurrentWorkers
	)
	if maxWorkers < 3 {
		maxWorkers = 3
	}
	wg.SetLimit(maxWorkers)
	for conn, userIds := range usersConns {
		tcon := conn
		tuserIds := userIds
		wg.Go(func() error {
			input := &msggateway.OnlineBatchPushOneMsgReq{MsgData: msg, PushToUserIDs: tuserIds}
			msgClient := msggateway.NewMsgGatewayClient(tcon)
			reply, err := msgClient.SuperGroupOnlineBatchPushOneMsg(ctx, input)
			if err != nil {
				return nil
			}
			log.ZDebug(ctx, "push result", "reply", reply)
			if reply != nil && reply.SinglePushResult != nil {
				mu.Lock()
				wsResults = append(wsResults, reply.SinglePushResult...)
				mu.Unlock()
			}
			return nil
		})
	}
	_ = wg.Wait()
	return wsResults, nil
}
func (p *Pusher) GetConnsAndOnlinePush(ctx context.Context, msg *sdkws.MsgData, pushToUserIDs []string) (wsResults []*msggateway.SingleMsgToUserResults, err error) {
	if p.config.Envs.Discovery == "k8s" {
		return p.k8sOnlinePush(ctx, msg, pushToUserIDs)
	}
	conns, err := p.discov.GetConns(ctx, p.config.RpcRegisterName.OpenImMessageGatewayName)
	log.ZDebug(ctx, "get gateway conn", "conn length", len(conns))
	if err != nil {
		return nil, err
	}

	var (
		mu         sync.Mutex
		wg         = errgroup.Group{}
		input      = &msggateway.OnlineBatchPushOneMsgReq{MsgData: msg, PushToUserIDs: pushToUserIDs}
		maxWorkers = p.config.Push.MaxConcurrentWorkers
	)

	if maxWorkers < 3 {
		maxWorkers = 3
	}

	wg.SetLimit(maxWorkers)

	// Online push message
	for _, conn := range conns {
		conn := conn // loop var safe
		wg.Go(func() error {
			msgClient := msggateway.NewMsgGatewayClient(conn)
			reply, err := msgClient.SuperGroupOnlineBatchPushOneMsg(ctx, input)
			if err != nil {
				return nil
			}

			log.ZDebug(ctx, "push result", "reply", reply)
			if reply != nil && reply.SinglePushResult != nil {
				mu.Lock()
				wsResults = append(wsResults, reply.SinglePushResult...)
				mu.Unlock()
			}

			return nil
		})
	}

	_ = wg.Wait()

	// always return nil
	return wsResults, nil
}

func (p *Pusher) offlinePushMsg(ctx context.Context, conversationID string, msg *sdkws.MsgData, offlinePushUserIDs []string) error {
	title, content, opts, err := p.getOfflinePushInfos(conversationID, msg)
	if err != nil {
		return err
	}
	err = p.offlinePusher.Push(ctx, offlinePushUserIDs, title, content, opts)
	if err != nil {
		prommetrics.MsgOfflinePushFailedCounter.Inc()
		return err
	}
	return nil
}

func (p *Pusher) GetOfflinePushOpts(msg *sdkws.MsgData) (opts *offlinepush.Opts, err error) {
	opts = &offlinepush.Opts{Signal: &offlinepush.Signal{}}
	// if msg.ContentType > constant.SignalingNotificationBegin && msg.ContentType < constant.SignalingNotificationEnd {
	// 	req := &sdkws.SignalReq{}
	// 	if err := proto.Unmarshal(msg.Content, req); err != nil {
	// 		return nil, utils.Wrap(err, "")
	// 	}
	// 	switch req.Payload.(type) {
	// 	case *sdkws.SignalReq_Invite, *sdkws.SignalReq_InviteInGroup:
	// 		opts.Signal = &offlinepush.Signal{ClientMsgID: msg.ClientMsgID}
	// 	}
	// }
	if msg.OfflinePushInfo != nil {
		opts.IOSBadgeCount = msg.OfflinePushInfo.IOSBadgeCount
		opts.IOSPushSound = msg.OfflinePushInfo.IOSPushSound
		opts.Ex = msg.OfflinePushInfo.Ex
	}
	return opts, nil
}

func (p *Pusher) getOfflinePushInfos(conversationID string, msg *sdkws.MsgData) (title, content string, opts *offlinepush.Opts, err error) {
	if p.offlinePusher == nil {
		err = errNoOfflinePusher
		return
	}

	type atContent struct {
		Text       string   `json:"text"`
		AtUserList []string `json:"atUserList"`
		IsAtSelf   bool     `json:"isAtSelf"`
	}

	opts, err = p.GetOfflinePushOpts(msg)
	if err != nil {
		return
	}

	if msg.OfflinePushInfo != nil {
		title = msg.OfflinePushInfo.Title
		content = msg.OfflinePushInfo.Desc
	}
	if title == "" {
		switch msg.ContentType {
		case constant.Text:
			fallthrough
		case constant.Picture:
			fallthrough
		case constant.Voice:
			fallthrough
		case constant.Video:
			fallthrough
		case constant.File:
			title = constant.ContentType2PushContent[int64(msg.ContentType)]
		case constant.AtText:
			ac := atContent{}
			_ = jsonutil.JsonStringToStruct(string(msg.Content), &ac)
			if datautil.Contain(conversationID, ac.AtUserList...) {
				title = constant.ContentType2PushContent[constant.AtText] + constant.ContentType2PushContent[constant.Common]
			} else {
				title = constant.ContentType2PushContent[constant.GroupMsg]
			}
		case constant.SignalingNotification:
			title = constant.ContentType2PushContent[constant.SignalMsg]
		default:
			title = constant.ContentType2PushContent[constant.Common]
		}
	}
	if content == "" {
		content = title
	}
	return
}
