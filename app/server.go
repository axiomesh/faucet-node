package app

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/axiomesh/faucet/global"
	"github.com/axiomesh/faucet/internal"
	"github.com/axiomesh/faucet/internal/utils"
	"github.com/axiomesh/faucet/pkg/loggers"
	"github.com/axiomesh/faucet/pkg/repo"
)

// 2. api：input： net，contractAddress，address； output：0，hash
// 3. 验证leveldb， key：address； value：[timestamp, net（eth，bxh），amount, contartAddress] , 每天发一个
// 4. 调用对应测试网交易
type Server struct {
	config *repo.Config
	router *gin.Engine
	logger logrus.FieldLogger
	client *internal.Client

	ctx    context.Context
	cancel context.CancelFunc
}

func NewServer(client *internal.Client, config *repo.Config) (*Server, error) {
	ctx, cancel := context.WithCancel(context.Background())
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	return &Server{
		config: config,
		router: router,
		client: client,
		ctx:    ctx,
		cancel: cancel,
		logger: loggers.Logger(loggers.ApiServer),
	}, nil
}

func (g *Server) Start() error {
	g.router.Use(gin.Recovery()).Use(cors.Default()).Use(g.MaxAllowed(200))
	v := g.router.Group("/faucet")
	{
		v.POST("directClaim", g.directClaim)
		v.POST("tweetClaim", g.tweetClaim)
		v.POST("preCheck", g.preCheck)
	}

	go func() {
		g.logger.Infoln("start gin success")
		err := g.router.Run(fmt.Sprintf(":%s", g.client.Config.Network.Port))
		if err != nil {
			g.logger.Error(err)
			panic(err)
		}
		<-g.ctx.Done()
	}()
	return nil
}

func (g *Server) directClaim(c *gin.Context) {
	var directClaimInput global.DirectClaimReq
	if err := c.BindJSON(&directClaimInput); err != nil {
		global.Result(global.Fail(global.ParseErrCode, global.ParseErrMsg), c)
		return
	}

	if judge := IsValidEthereumAddress(directClaimInput.Address); !judge {
		global.Result(global.Fail(global.ErrAddrCode, global.ErrAddrMsg+fmt.Sprintf(directClaimInput.Address)), c)
		return
	}

	if !strings.EqualFold(g.config.Axiom.TestNetName, directClaimInput.Net) {
		global.Result(global.Fail(global.NotSupportCode, global.NotSupportMsg+fmt.Sprintf(directClaimInput.Net)), c)
		return
	}

	g.client.GinContext = c
	txHash, code, err := g.client.SendTra(directClaimInput.Net, directClaimInput.Address, g.client.Config.Axiom.Amount, "")
	if err == nil || err.Error() != global.AddrPreLockErrMsg {
		internal.DeleteTxData(g.client, strings.ToLower(directClaimInput.Address), global.NativeToken, directClaimInput.Net)
	}
	if err != nil {
		global.Result(global.Fail(code, err.Error()), c)
		return
	}

	global.Result(global.Success(txHash), c)
}

func (g *Server) tweetClaim(c *gin.Context) {
	var tweetClaimReq global.TweetClaimReq
	if err := c.BindJSON(&tweetClaimReq); err != nil {
		global.Result(global.Fail(global.ParseErrCode, global.ParseErrMsg), c)
		return
	}

	if judge := IsValidEthereumAddress(tweetClaimReq.Address); !judge {
		global.Result(global.Fail(global.ErrAddrCode, global.ErrAddrMsg+fmt.Sprintf(tweetClaimReq.Address)), c)
		return
	}

	if !strings.EqualFold(g.config.Axiom.TestNetName, tweetClaimReq.Net) {
		global.Result(global.Fail(global.NotSupportCode, global.NotSupportMsg+fmt.Sprintf(tweetClaimReq.Net)), c)
		return
	}
	if judge := isValidTwitterURL(tweetClaimReq.TweetUrl); !judge {
		global.Result(global.Fail(global.TweetUrlErrCode, global.TweetUrlErrMsg), c)
		return
	}

	g.client.GinContext = c
	txHash, code, err := g.client.SendTra(tweetClaimReq.Net, tweetClaimReq.Address, g.client.Config.Axiom.TweetAmount, tweetClaimReq.TweetUrl)
	if err == nil || err.Error() != global.AddrPreLockErrMsg {
		internal.DeleteTxData(g.client, strings.ToLower(tweetClaimReq.Address), global.NativeToken, tweetClaimReq.Net)
	}
	if err != nil {
		global.Result(global.Fail(code, err.Error()), c)
		return
	}

	global.Result(global.Success(txHash), c)
}

func (g *Server) preCheck(c *gin.Context) {
	var preCheckReq global.PreCheckReq
	if err := c.BindJSON(&preCheckReq); err != nil {
		global.Result(global.Fail(global.ParseErrCode, global.ParseErrMsg), c)
		return
	}

	if judge := IsValidEthereumAddress(preCheckReq.Address); !judge {
		global.Result(global.Fail(global.ErrAddrCode, global.ErrAddrMsg+fmt.Sprintf(preCheckReq.Address)), c)
		return
	}

	if !strings.EqualFold(g.config.Axiom.TestNetName, preCheckReq.Net) {
		global.Result(global.Fail(global.NotSupportCode, global.NotSupportMsg+fmt.Sprintf(preCheckReq.Net)), c)
		return
	}

	code, err := g.client.PreCheck(preCheckReq.Net, preCheckReq.Address)
	if err != nil {
		global.Result(global.Fail(code, err.Error()), c)
		return
	}

	global.Result(global.Success("PreCheck Pass"), c)
}

func (g *Server) Stop() error {
	g.client.Close()
	g.cancel()
	g.logger.Infoln("gin service stop")
	return nil
}

// MaxAllowed 限流器
func (g *Server) MaxAllowed(limitValue int64) func(c *gin.Context) {
	limiter := utils.NewLimiter(limitValue)
	g.logger.Infof("limiter.SetMax: %d", limitValue)
	// 返回限流逻辑
	return func(c *gin.Context) {
		if !limiter.Ok() {
			c.AbortWithStatus(http.StatusServiceUnavailable) // 超过每秒200，就返回503错误码
			return
		}
		c.Next()
	}
}

func IsValidEthereumAddress(address string) bool {
	// 正则表达式模式匹配以太坊地址
	pattern := "^0x[0-9a-fA-F]{40}$"
	regex := regexp.MustCompile(pattern)
	return regex.MatchString(address)
}

func isValidTwitterURL(url string) bool {
	twitterURLPattern := `^(https?://(twitter\.com|x\.com)/[a-zA-Z0-9_]+/status/\d+).*`
	re := regexp.MustCompile(twitterURLPattern)
	return re.MatchString(url)
}
