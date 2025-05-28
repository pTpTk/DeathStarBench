package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"os"
	"strconv"
	"time"

	"github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/registry"
	"github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/services/user"
	"github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/tracing"
	"github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/tune"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"fmt"
	"context"
)

func main() {
	tune.Init()
	// initializeDatabase()
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}).With().Timestamp().Caller().Logger()

	log.Info().Msg("Reading config...")
	jsonFile, err := os.Open("config.json")
	if err != nil {
		log.Error().Msgf("Got error while reading config: %v", err)
	}

	defer jsonFile.Close()

	byteValue, _ := ioutil.ReadAll(jsonFile)

	var result map[string]string
	json.Unmarshal([]byte(byteValue), &result)

	log.Info().Msg("Initializing DB connection...")
	mongoClient, mongoClose := initializeDatabase(result["UserMongoAddress"])
	defer mongoClose()

	dynamoTest()

	servPort, _ := strconv.Atoi(result["UserPort"])
	servIP := result["UserIP"]

	var (
		jaegerAddr = flag.String("jaegeraddr", result["jaegerAddress"], "Jaeger address")
		consulAddr = flag.String("consuladdr", result["consulAddress"], "Consul address")
	)
	flag.Parse()

	log.Info().Msgf("Initializing jaeger agent [service name: %v | host: %v]...", "user", *jaegerAddr)
	tracer, err := tracing.Init("user", *jaegerAddr)
	if err != nil {
		log.Panic().Msgf("Got error while initializing jaeger agent: %v", err)
	}
	log.Info().Msg("Jaeger agent initialized")

	log.Info().Msgf("Initializing consul agent [host: %v]...", *consulAddr)
	registry, err := registry.NewClient(*consulAddr)
	if err != nil {
		log.Panic().Msgf("Got error while initializing consul agent: %v", err)
	}
	log.Info().Msg("Consul agent initialized")

	srv := &user.Server{
		Port:        servPort,
		IpAddr:      servIP,
		Tracer:      tracer,
		Registry:    registry,
		MongoClient: mongoClient,
	}

	log.Info().Msg("Starting server...")
	log.Fatal().Msg(srv.Run().Error())
}

func dynamoTest(){
	cfg, err := config.LoadDefaultConfig(context.TODO())
    if err != nil {
        panic(err)
    }

    svc := dynamodb.NewFromConfig(cfg)
    p := dynamodb.NewListTablesPaginator(svc, nil, func(o *dynamodb.ListTablesPaginatorOptions) {
        o.StopOnDuplicateToken = true
    })

    for p.HasMorePages() {
        out, err := p.NextPage(context.TODO())
        if err != nil {
            panic(err)
        }

        for _, tn := range out.TableNames {
            fmt.Println(tn)
        }
    }
}
