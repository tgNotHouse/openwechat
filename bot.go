package openwechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"runtime"
	"sync"

	goqrcode "github.com/skip2/go-qrcode"
)

type Bot struct {
	ScanCallBack        func(body []byte)            // 扫码回调,可获取扫码用户的头像
	LoginCallBack       func(body []byte)            // 登陆回调
	LogoutCallBack      func(bot *Bot)               // 退出回调
	UUIDCallback        func(uuid string)            // 获取UUID的回调函数
	SyncCheckCallback   func(resp SyncCheckResponse) // 心跳回调
	MessageHandler      MessageHandler               // 获取消息成功的handle
	MessageErrorHandler func(err error) bool         // 获取消息发生错误的handle, 返回true则尝试继续监听
	once                sync.Once
	err                 error
	context             context.Context
	cancel              context.CancelFunc
	Caller              *Caller
	self                *Self
	Storage             *Storage
	hotReloadStorage    HotReloadStorage
	uuid                string
	deviceId            string // 设备Id
}

// Alive 判断当前用户是否正常在线
func (b *Bot) Alive() bool {
	if b.self == nil {
		return false
	}
	select {
	case <-b.context.Done():
		return false
	default:
		return true
	}
}

// SetDeviceId
// @description: 设置设备Id
// @receiver b
// @param deviceId
func (b *Bot) SetDeviceId(deviceId string) {
	b.deviceId = deviceId
}

// GetCurrentUser 获取当前的用户
//
//	self, err := bot.GetCurrentUser()
//	if err != nil {
//		return
//	}
//	fmt.Println(self.NickName)
func (b *Bot) GetCurrentUser() (*Self, error) {
	if b.self == nil {
		return nil, errors.New("user not login")
	}
	return b.self, nil
}

func (b *Bot) login(login BotLogin) error {
	return login.Login(b)
}

// Login 用户登录
func (b *Bot) Login() error {
	scanLogin := &SacnLogin{}
	return b.login(scanLogin)
}

// HotLogin 热登录,可实现在单位时间内免重复扫码登录
func (b *Bot) HotLogin(storage HotReloadStorage, opts ...HotLoginOptionFunc) error {
	hotLogin := &HotLogin{storage: storage}
	// 进行相关设置。
	// 如果相对默认的行为进行修改，在opts里面进行追加即可。
	opts = append(defaultHotLoginOpts[:], opts...)
	for _, opt := range opts {
		opt(&hotLogin.opt)
	}
	return b.login(hotLogin)
}

// PushLogin 免扫码登录
// 免扫码登录需要先扫码登录一次才可以进行扫码登录
// 扫码登录成功后需要利用微信号发送一条消息，然后在手机上进行主动退出。
// 这时候在进行一次 PushLogin 即可。
func (b *Bot) PushLogin(storage HotReloadStorage, opts ...PushLoginOptionFunc) error {
	pushLogin := &PushLogin{storage: storage}
	// 进行相关设置。
	// 如果相对默认的行为进行修改，在opts里面进行追加即可。
	opts = append(defaultPushLoginOpts[:], opts...)
	for _, opt := range opts {
		opt(&pushLogin.opt)
	}
	return b.login(pushLogin)
}

// Logout 用户退出
func (b *Bot) Logout() error {
	if b.Alive() {
		info := b.Storage.LoginInfo
		if err := b.Caller.Logout(info); err != nil {
			return err
		}
		b.Exit()
		return nil
	}
	return errors.New("user not login")
}

// HandleLogin 登录逻辑
func (b *Bot) HandleLogin(data []byte) error {
	// 获取登录的一些基本的信息
	info, err := b.Caller.GetLoginInfo(data)
	if err != nil {
		return err
	}
	// 将LoginInfo存到storage里面
	b.Storage.LoginInfo = info

	// 处理设备Id
	if b.deviceId == "" {
		b.deviceId = GetRandomDeviceId()
	}

	// 构建BaseRequest
	request := &BaseRequest{
		Uin:      info.WxUin,
		Sid:      info.WxSid,
		Skey:     info.SKey,
		DeviceID: b.deviceId,
	}

	// 将BaseRequest存到storage里面方便后续调用
	b.Storage.Request = request

	// 如果是热登陆,则将当前的重要信息写入hotReloadStorage
	if b.hotReloadStorage != nil {
		if err = b.DumpHotReloadStorage(); err != nil {
			return err
		}
	}

	return b.WebInit()
}

// WebInit 根据有效凭证获取和初始化用户信息
func (b *Bot) WebInit() error {
	req := b.Storage.Request
	info := b.Storage.LoginInfo
	// 获取初始化的用户信息和一些必要的参数
	resp, err := b.Caller.WebInit(req)
	if err != nil {
		return err
	}
	// 设置当前的用户
	b.self = &Self{bot: b, User: &resp.User}
	b.self.formatEmoji()
	b.self.self = b.self
	b.Storage.Response = resp

	// 通知手机客户端已经登录
	if err = b.Caller.WebWxStatusNotify(req, resp, info); err != nil {
		return err
	}
	// 开启协程，轮询获取是否有新的消息返回

	// FIX: 当bot在线的情况下执行热登录,会开启多次事件监听
	go b.once.Do(func() {
		if b.MessageErrorHandler == nil {
			b.MessageErrorHandler = defaultSyncCheckErrHandler(b)
		}
		for {
			err := b.syncCheck()
			if err == nil {
				continue
			}
			// 判断是否继续, 如果不继续则退出
			if goon := b.MessageErrorHandler(err); !goon {
				b.err = err
				break
			}
		}
	})
	return nil
}

// 轮询请求
// 根据状态码判断是否有新的请求
func (b *Bot) syncCheck() error {
	var (
		err  error
		resp *SyncCheckResponse
	)
	for b.Alive() {
		// 长轮询检查是否有消息返回
		resp, err = b.Caller.SyncCheck(b.Storage.Request, b.Storage.LoginInfo, b.Storage.Response)
		if err != nil {
			return err
		}
		// 执行心跳回调
		if b.SyncCheckCallback != nil {
			b.SyncCheckCallback(*resp)
		}
		// 如果不是正常的状态码返回，发生了错误，直接退出
		if !resp.Success() {
			return resp.Err()
		}
		// 如果Selector不为0，则获取消息
		if !resp.NorMal() {
			messages, err := b.syncMessage()
			if err != nil {
				return err
			}
			if b.MessageHandler == nil {
				continue
			}
			for _, message := range messages {
				message.init(b)
				// 默认同步调用
				// 如果异步调用则需自行处理
				// 如配合 openwechat.MessageMatchDispatcher 使用
				b.MessageHandler(message)
			}
		}
	}
	return err
}

// 获取新的消息
func (b *Bot) syncMessage() ([]*Message, error) {
	resp, err := b.Caller.WebWxSync(b.Storage.Request, b.Storage.Response, b.Storage.LoginInfo)
	if err != nil {
		return nil, err
	}
	// 更新SyncKey并且重新存入storage
	b.Storage.Response.SyncKey = resp.SyncKey
	return resp.AddMsgList, nil
}

// Block 当消息同步发生了错误或者用户主动在手机上退出，该方法会立即返回，否则会一直阻塞
func (b *Bot) Block() error {
	if b.self == nil {
		return errors.New("`Block` must be called after user login")
	}
	<-b.Context().Done()
	return nil
}

// Exit 主动退出，让 Block 不在阻塞
func (b *Bot) Exit() {
	if b.LogoutCallBack != nil {
		b.LogoutCallBack(b)
	}
	b.self = nil
	b.cancel()
}

// CrashReason 获取当前Bot崩溃的原因
func (b *Bot) CrashReason() error {
	return b.err
}

// DumpHotReloadStorage 写入HotReloadStorage
func (b *Bot) DumpHotReloadStorage() error {
	if b.hotReloadStorage == nil {
		return errors.New("HotReloadStorage can not be nil")
	}
	return b.DumpTo(b.hotReloadStorage)
}

// DumpTo 将热登录需要的数据写入到指定的 io.Writer 中
// 注: 写之前最好先清空之前的数据
func (b *Bot) DumpTo(writer io.Writer) error {
	cookies := b.Caller.Client.GetCookieJar()
	item := HotReloadStorageItem{
		BaseRequest:  b.Storage.Request,
		Jar:          cookies,
		LoginInfo:    b.Storage.LoginInfo,
		WechatDomain: b.Caller.Client.Domain,
		UUID:         b.uuid,
	}
	return json.NewEncoder(writer).Encode(item)
}

// IsHot returns true if is hot login otherwise false
func (b *Bot) IsHot() bool {
	return b.hotReloadStorage != nil
}

// UUID returns current uuid of bot
func (b *Bot) UUID() string {
	return b.uuid
}

// Context returns current context of bot
func (b *Bot) Context() context.Context {
	return b.context
}

func (b *Bot) reload() error {
	if b.hotReloadStorage == nil {
		return errors.New("hotReloadStorage is nil")
	}
	var item HotReloadStorageItem
	err := json.NewDecoder(b.hotReloadStorage).Decode(&item)
	if err != nil {
		return err
	}
	b.Caller.Client.Jar = item.Jar.AsCookieJar()
	b.Storage.LoginInfo = item.LoginInfo
	b.Storage.Request = item.BaseRequest
	b.Caller.Client.Domain = item.WechatDomain
	b.uuid = item.UUID
	return nil
}

// NewBot Bot的构造方法
// 接收外部的 context.Context，用于控制Bot的存活
func NewBot(c context.Context) *Bot {
	caller := DefaultCaller()
	// 默认行为为网页版微信模式
	caller.Client.SetMode(normal)
	ctx, cancel := context.WithCancel(c)
	return &Bot{Caller: caller, Storage: &Storage{}, context: ctx, cancel: cancel}
}

// DefaultBot 默认的Bot的构造方法,
// mode不传入默认为 openwechat.Normal,详情见mode
//
//	bot := openwechat.DefaultBot(openwechat.Desktop)
func DefaultBot(opts ...BotOptionFunc) *Bot {
	bot := NewBot(context.Background())
	// 获取二维码回调
	bot.UUIDCallback = PrintlnQrcodeUrl
	// 扫码回调
	bot.ScanCallBack = func(body []byte) {
		log.Println("扫码成功,请在手机上确认登录")
	}
	// 登录回调
	bot.LoginCallBack = func(body []byte) {
		log.Println("登录成功")
	}
	// 心跳回调函数
	// 默认的行为打印SyncCheckResponse
	bot.SyncCheckCallback = func(resp SyncCheckResponse) {
		log.Printf("RetCode:%s  Selector:%s", resp.RetCode, resp.Selector)
	}
	for _, opt := range opts {
		opt(bot)
	}
	return bot
}

// defaultSyncCheckErrHandler 默认的SyncCheck错误处理函数
func defaultSyncCheckErrHandler(bot *Bot) func(error) bool {
	return func(err error) bool {
		var ret Ret
		if errors.As(err, &ret) {
			switch ret {
			case failedLoginCheck, cookieInvalid, failedLoginWarn:
				_ = bot.Logout()
				return false
			}
		}
		return true
	}
}

// GetQrcodeUrl 通过uuid获取登录二维码的url
func GetQrcodeUrl(uuid string) string {
	return qrcode + uuid
}

// PrintlnQrcodeUrl 打印登录二维码
func PrintlnQrcodeUrl(uuid string) {
	println("访问下面网址扫描二维码登录")
	qrcodeUrl := GetQrcodeUrl(uuid)
	println(qrcodeUrl)

	// browser open the login url
	_ = open(qrcodeUrl)
}

// open opens the specified URL in the default browser of the user.
func open(url string) error {
	var (
		cmd  string
		args []string
	)

	switch runtime.GOOS {
	case "windows":
		cmd, args = "cmd", []string{"/c", "start"}
	case "darwin":
		cmd = "open"
	default:
		// "linux", "freebsd", "openbsd", "netbsd"
		cmd = "xdg-open"
	}
	args = append(args, url)
	return exec.Command(cmd, args...).Start()
}

// PrintlnConsoleQrcode 终端打印登录二维码
func PrintlnConsoleQrcode(uuid string) {
	qrcodeUrl := GetQrcodeUrl(uuid)
	fmt.Printf("扫描终端二维码或浏览器打开连接扫码登录: \n%s\n\n", qrcodeUrl)

	qr, err := goqrcode.New(qrcodePreifx+uuid, goqrcode.High)
	if err != nil {
		panic(err)
	}

	fmt.Println(qr.ToSmallString(false))
}
