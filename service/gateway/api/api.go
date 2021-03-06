// Package api is an API Gateway
package api

import (
	"fmt"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/stack-labs/stack-rpc"
	ahandler "github.com/stack-labs/stack-rpc/api/handler"
	aapi "github.com/stack-labs/stack-rpc/api/handler/api"
	"github.com/stack-labs/stack-rpc/api/handler/event"
	ahttp "github.com/stack-labs/stack-rpc/api/handler/http"
	arpc "github.com/stack-labs/stack-rpc/api/handler/rpc"
	"github.com/stack-labs/stack-rpc/api/handler/web"
	"github.com/stack-labs/stack-rpc/api/resolver"
	"github.com/stack-labs/stack-rpc/api/resolver/grpc"
	"github.com/stack-labs/stack-rpc/api/resolver/host"
	"github.com/stack-labs/stack-rpc/api/resolver/path"
	rrstack "github.com/stack-labs/stack-rpc/api/resolver/stack"
	"github.com/stack-labs/stack-rpc/api/router"
	regRouter "github.com/stack-labs/stack-rpc/api/router/registry"
	"github.com/stack-labs/stack-rpc/api/server"
	"github.com/stack-labs/stack-rpc/api/server/acme"
	"github.com/stack-labs/stack-rpc/api/server/acme/autocert"
	httpapi "github.com/stack-labs/stack-rpc/api/server/http"
	"github.com/stack-labs/stack-rpc/pkg/cli"
	"github.com/stack-labs/stack-rpc/util/log"

	"github.com/stack-labs/stack-rpc-plugins/service/gateway/handler"
	"github.com/stack-labs/stack-rpc-plugins/service/gateway/helper"
	"github.com/stack-labs/stack-rpc-plugins/service/gateway/plugin"
	"github.com/stack-labs/stack-rpc-plugins/service/gateway/stats"
)

// basic vars
var (
	Name                  = "stack.rpc.gateway"
	Address               = ":8080"
	Handler               = "meta"
	Resolver              = "stack"
	RPCPath               = "/rpc"
	APIPath               = "/"
	ProxyPath             = "/{service:[a-zA-Z0-9]+}"
	Namespace             = "stack.rpc.api"
	HeaderPrefix          = "X-Stack-"
	EnableRPC             = false
	ACMEProvider          = "autocert"
	ACMEChallengeProvider = "cloudflare"
	ACMECA                = acme.LetsEncryptProductionCA
)

// run api gateway
func Run(ctx *cli.Context, service stack.Service) ([]stack.Option, error) {
	if len(ctx.GlobalString("server_name")) > 0 {
		Name = ctx.GlobalString("server_name")
	}
	if len(ctx.String("address")) > 0 {
		Address = ctx.String("address")
	}
	if len(ctx.String("handler")) > 0 {
		Handler = ctx.String("handler")
	}
	if len(ctx.String("namespace")) > 0 {
		Namespace = ctx.String("namespace")
	}
	if len(ctx.String("resolver")) > 0 {
		Resolver = ctx.String("resolver")
	}
	if len(ctx.String("enable_rpc")) > 0 {
		EnableRPC = ctx.Bool("enable_rpc")
	}
	if len(ctx.GlobalString("acme_provider")) > 0 {
		ACMEProvider = ctx.GlobalString("acme_provider")
	}

	// Init plugins
	for _, p := range plugin.Plugins() {
		p.Init(ctx)
	}

	// Init API
	var opts []server.Option

	if ctx.GlobalBool("enable_acme") {
		hosts := helper.ACMEHosts(ctx)
		opts = append(opts, server.EnableACME(true))
		opts = append(opts, server.ACMEHosts(hosts...))
		switch ACMEProvider {
		case "autocert":
			opts = append(opts, server.ACMEProvider(autocert.New()))
		default:
			log.Fatalf("%s is not a valid ACME provider\n", ACMEProvider)
		}
	} else if ctx.GlobalBool("enable_tls") {
		config, err := helper.TLSConfig(ctx)
		if err != nil {
			fmt.Println(err.Error())
			return nil, err
		}

		opts = append(opts, server.EnableTLS(true))
		opts = append(opts, server.TLSConfig(config))
	}

	// create the router
	var h http.Handler
	r := mux.NewRouter()
	h = r

	if ctx.GlobalBool("enable_stats") {
		st := stats.New()
		r.HandleFunc("/stats", st.StatsHandler)
		h = st.ServeHTTP(r)
		st.Start()
		defer st.Stop()
	}

	// return version and list of services
	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		helper.ServeCORS(w, r)

		if r.Method == "OPTIONS" {
			return
		}

		response := fmt.Sprintf(`{"version": "%s"}`, ctx.App.Version)
		w.Write([]byte(response))
	})

	// strip favicon.ico
	r.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {})

	// srvOpts = append(srvOpts, stack.Name(Name))
	// if i := time.Duration(ctx.GlobalInt("register_ttl")); i > 0 {
	// 	srvOpts = append(srvOpts, stack.RegisterTTL(i*time.Second))
	// }
	// if i := time.Duration(ctx.GlobalInt("register_interval")); i > 0 {
	// 	srvOpts = append(srvOpts, stack.RegisterInterval(i*time.Second))
	// }

	// initialise service
	// service := stack.NewService(srvOpts...)

	// register rpc handler
	if EnableRPC {
		log.Logf("Registering RPC Handler at %s", RPCPath)
		r.Handle(RPCPath, handler.NewRPCHandlerFunc(service.Options()))
	}

	// resolver options
	ropts := []resolver.Option{
		resolver.WithNamespace(Namespace),
		resolver.WithHandler(Handler),
	}

	// default resolver
	rr := rrstack.NewResolver(ropts...)

	switch Resolver {
	case "host":
		rr = host.NewResolver(ropts...)
	case "path":
		rr = path.NewResolver(ropts...)
	case "grpc":
		rr = grpc.NewResolver(ropts...)
	}

	switch Handler {
	case "rpc":
		log.Logf("Registering API RPC Handler at %s", APIPath)
		rt := regRouter.NewRouter(
			router.WithNamespace(Namespace),
			router.WithHandler(arpc.Handler),
			router.WithResolver(rr),
			router.WithRegistry(service.Options().Registry),
		)
		rp := arpc.NewHandler(
			ahandler.WithNamespace(Namespace),
			ahandler.WithRouter(rt),
			ahandler.WithService(service),
		)
		r.PathPrefix(APIPath).Handler(rp)
	case "api":
		log.Logf("Registering API Request Handler at %s", APIPath)
		rt := regRouter.NewRouter(
			router.WithNamespace(Namespace),
			router.WithHandler(aapi.Handler),
			router.WithResolver(rr),
			router.WithRegistry(service.Options().Registry),
		)
		ap := aapi.NewHandler(
			ahandler.WithNamespace(Namespace),
			ahandler.WithRouter(rt),
			ahandler.WithService(service),
		)
		r.PathPrefix(APIPath).Handler(ap)
	case "event":
		log.Logf("Registering API Event Handler at %s", APIPath)
		rt := regRouter.NewRouter(
			router.WithNamespace(Namespace),
			router.WithHandler(event.Handler),
			router.WithResolver(rr),
			router.WithRegistry(service.Options().Registry),
		)
		ev := event.NewHandler(
			ahandler.WithNamespace(Namespace),
			ahandler.WithRouter(rt),
			ahandler.WithService(service),
		)
		r.PathPrefix(APIPath).Handler(ev)
	case "http", "proxy":
		log.Logf("Registering API HTTP Handler at %s", ProxyPath)
		rt := regRouter.NewRouter(
			router.WithNamespace(Namespace),
			router.WithHandler(ahttp.Handler),
			router.WithResolver(rr),
			router.WithRegistry(service.Options().Registry),
		)
		ht := ahttp.NewHandler(
			ahandler.WithNamespace(Namespace),
			ahandler.WithRouter(rt),
			ahandler.WithService(service),
		)
		r.PathPrefix(ProxyPath).Handler(ht)
	case "web":
		log.Logf("Registering API Web Handler at %s", APIPath)
		rt := regRouter.NewRouter(
			router.WithNamespace(Namespace),
			router.WithHandler(web.Handler),
			router.WithResolver(rr),
			router.WithRegistry(service.Options().Registry),
		)
		w := web.NewHandler(
			ahandler.WithNamespace(Namespace),
			ahandler.WithRouter(rt),
			ahandler.WithService(service),
		)
		r.PathPrefix(APIPath).Handler(w)
	default:
		log.Logf("Registering API Default Handler at %s", APIPath)
		rt := regRouter.NewRouter(
			router.WithNamespace(Namespace),
			router.WithResolver(rr),
			router.WithRegistry(service.Options().Registry),
		)
		r.PathPrefix(APIPath).Handler(handler.Meta(service, rt))
	}

	// reverse wrap handler
	plugins := append(plugin.Plugins(), plugin.Plugins()...)
	for i := len(plugins); i > 0; i-- {
		h = plugins[i-1].Handler()(h)
	}

	// create the server
	api := httpapi.NewServer(Address)
	api.Init(opts...)
	api.Handle("/", h)

	// Start API
	if err := api.Start(); err != nil {
		log.Fatal(err)
	}

	options := []stack.Option{
		stack.AfterStop(func() error {
			log.Infof("api stop")
			return api.Stop()
		}),
	}
	return options, nil
}

// api gateway options
func Options() (options []stack.Option) {
	flags := []cli.Flag{
		cli.StringFlag{
			Name:   "address",
			Usage:  "Set the api address e.g 0.0.0.0:8080",
			EnvVar: "MICRO_API_ADDRESS",
		},
		cli.StringFlag{
			Name:   "handler",
			Usage:  "Specify the request handler to be used for mapping HTTP requests to services; {api, event, http, rpc}",
			EnvVar: "MICRO_API_HANDLER",
		},
		cli.StringFlag{
			Name:   "namespace",
			Usage:  "Set the namespace used by the API e.g. com.example.api",
			EnvVar: "MICRO_API_NAMESPACE",
		},
		cli.StringFlag{
			Name:   "resolver",
			Usage:  "Set the hostname resolver used by the API {host, path, grpc}",
			EnvVar: "MICRO_API_RESOLVER",
		},
		cli.BoolFlag{
			Name:   "enable_rpc",
			Usage:  "Enable call the backend directly via /rpc",
			EnvVar: "MICRO_API_ENABLE_RPC",
		},
	}

	options = append(options, stack.Flags(flags...))

	return
}
