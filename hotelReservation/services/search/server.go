package search

import (
	"fmt"
	"net"
	"time"
	"strconv"
	"net/http"
	"io/ioutil"
	"encoding/json"
	"os"

	"github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/dialer"
	"github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/registry"
	geo "github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/services/geo/proto"
	rate "github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/services/rate/proto"
	pb "github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/services/search/proto"
	"github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/tls"
	"github.com/google/uuid"
	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	_ "github.com/mbobakov/grpc-consul-resolver"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/rs/zerolog/log"
	context "golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

const name = "srv-search"

// Server implments the search service
type Server struct {
	pb.UnimplementedSearchServer

	geoClient  geo.GeoClient
	rateClient rate.RateClient
	uuid       string

	Tracer     opentracing.Tracer
	Port       int
	IpAddr     string
	ConsulAddr string
	KnativeDns string
	Registry   *registry.Client
}

// Run starts the server
func (s *Server) Run() error {
	if s.Port == 0 {
		return fmt.Errorf("server port must be set")
	}

	s.uuid = uuid.New().String()

	opts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Timeout: 120 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			PermitWithoutStream: true,
		}),
		grpc.UnaryInterceptor(
			otgrpc.OpenTracingServerInterceptor(s.Tracer),
		),
	}

	if tlsopt := tls.GetServerOpt(); tlsopt != nil {
		opts = append(opts, tlsopt)
	}

	srv := grpc.NewServer(opts...)
	pb.RegisterSearchServer(srv, s)

	// init grpc clients
	if err := s.initGeoClient("srv-geo"); err != nil {
		return err
	}
	if err := s.initRateClient("srv-rate"); err != nil {
		return err
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.Port))
	if err != nil {
		log.Fatal().Msgf("failed to listen: %v", err)
	}

	err = s.Registry.Register(name, s.uuid, s.IpAddr, s.Port)
	if err != nil {
		return fmt.Errorf("failed register: %v", err)
	}
	log.Info().Msg("Successfully registered in consul")

	return srv.Serve(lis)
}

// Shutdown cleans up any processes
func (s *Server) Shutdown() {
	s.Registry.Deregister(s.uuid)
}

func (s *Server) initGeoClient(name string) error {
	conn, err := s.getGprcConn(name)
	if err != nil {
		return fmt.Errorf("dialer error: %v", err)
	}
	s.geoClient = geo.NewGeoClient(conn)
	return nil
}

func (s *Server) initRateClient(name string) error {
	conn, err := s.getGprcConn(name)
	if err != nil {
		return fmt.Errorf("dialer error: %v", err)
	}
	s.rateClient = rate.NewRateClient(conn)
	return nil
}

func (s *Server) getGprcConn(name string) (*grpc.ClientConn, error) {
	if s.KnativeDns != "" {
		return dialer.Dial(
			fmt.Sprintf("consul://%s/%s.%s", s.ConsulAddr, name, s.KnativeDns),
			dialer.WithTracer(s.Tracer))
	} else {
		return dialer.Dial(
			fmt.Sprintf("consul://%s/%s", s.ConsulAddr, name),
			dialer.WithTracer(s.Tracer),
			dialer.WithBalancer(s.Registry.Client),
		)
	}
}

// Nearby returns ids of nearby hotels ordered by ranking algo
func (s *Server) Nearby(ctx context.Context, req *pb.NearbyRequest) (*pb.SearchResult, error) {
	// find nearby hotels
	log.Trace().Msg("in Search Nearby")

	log.Trace().Msgf("nearby lat = %f", req.Lat)
	log.Trace().Msgf("nearby lon = %f", req.Lon)

	nearby, err := s.GeoHandler(ctx, &geo.Request{
		Lat: req.Lat,
		Lon: req.Lon,
	})
	if err != nil {
		return nil, err
	}

	for _, hid := range nearby.HotelIds {
		log.Trace().Msgf("get Nearby hotelId = %s", hid)
	}

	// find rates for hotels
	rates, err := s.RateHandler(ctx, &rate.Request{
		HotelIds: nearby.HotelIds,
		InDate:   req.InDate,
		OutDate:  req.OutDate,
	})
	if err != nil {
		return nil, err
	}

	// TODO(hw): add simple ranking algo to order hotel ids:
	// * geo distance
	// * price (best discount?)
	// * reviews

	// build the response
	res := new(pb.SearchResult)
	for _, ratePlan := range rates.RatePlans {
		log.Trace().Msgf("get RatePlan HotelId = %s, Code = %s", ratePlan.HotelId, ratePlan.Code)
		res.HotelIds = append(res.HotelIds, ratePlan.HotelId)
	}
	return res, nil
}

type HandlerType int
const (
	KUBERNETES = iota
	LAMBDA
)

func (s *Server) decideHandlerType() HandlerType {
	return LAMBDA
}

func (s *Server) GeoHandler(ctx context.Context, req *geo.Request) (*geo.Result, error) {
	if s.decideHandlerType() == KUBERNETES {
		return s.geoClient.Nearby(ctx, req)
	}

	baseURL := os.Getenv("GEO_AWS_URL")
	url := baseURL + "?lat=" + strconv.FormatFloat(float64(req.Lat),'f',-1,32) + 
		"&lon=" + strconv.FormatFloat(float64(req.Lon),'f',-1,32)
	fmt.Println(url)

	var out geo.Result
	err := s.invokeLambda(url, &out)
	return &out, err
}

func (s *Server) RateHandler(ctx context.Context, req *rate.Request) (*rate.Result, error) {
	if s.decideHandlerType() == KUBERNETES {
		return s.rateClient.GetRates(ctx, req)
	}

	baseURL := os.Getenv("RATE_AWS_URL")
	url := baseURL + "?"
	for _, h := range req.HotelIds {
		url += ("hotelIds=" + h + "&")
	}
	url += ("inDate=" + req.InDate + "&outDate=" + req.OutDate)
	fmt.Println(url)

	var out rate.Result
	err := s.invokeLambda(url, &out)
	return &out, err
}

func (s *Server) invokeLambda(url string, out interface{}) error {
	resp, err := http.Get(url)
	if err != nil {
		fmt.Println("Error creating request:", err)
		return err
	}
	defer resp.Body.Close()
    body, err := ioutil.ReadAll(resp.Body)

	fmt.Println("resp body: ", string(body))
	err = json.Unmarshal(body, out)
	if err != nil {
		fmt.Println("Error unmarshaling result, ", err)
		return err
	}

	return nil
}