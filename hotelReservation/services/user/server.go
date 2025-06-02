package user

import (
	"crypto/sha256"
	"fmt"
	"net"
	"time"

	"github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/registry"
	pb "github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/services/user/proto"
	"github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/tls"
	"github.com/google/uuid"
	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	"github.com/opentracing/opentracing-go"
	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
)

const name = "srv-user"

// Server implements the user service
type Server struct {
	pb.UnimplementedUserServer

	users map[string]string
	uuid  string

	Tracer      opentracing.Tracer
	Registry    *registry.Client
	Port        int
	IpAddr      string
	MongoClient *mongo.Client
	DynamoClient *dynamodb.Client
}

// Run starts the server
func (s *Server) Run() error {
	if s.Port == 0 {
		return fmt.Errorf("server port must be set")
	}

	// if s.users == nil {
	// 	s.users = loadUsers(s.MongoClient)
	// }

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

	pb.RegisterUserServer(srv, s)

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

// CheckUser returns whether the username and password are correct.
func (s *Server) CheckUser(ctx context.Context, req *pb.Request) (*pb.Result, error) {
	res := new(pb.Result)
	res.Correct = false

	log.Trace().Msg("CheckUser")

	sum := sha256.Sum256([]byte(req.Password))
	pass := fmt.Sprintf("%x", sum)

	curr, err := s.DynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("User_User"),
		Key: map[string]types.AttributeValue{
            "username": &types.AttributeValueMemberS{Value: req.Username},
        },
	})
	if err != nil {
		log.Error().Msgf("", err)
	}

	// username not found
	if len(curr.Item) == 0 {
		return res, nil
	}

	var user User
	err = attributevalue.UnmarshalMap(curr.Item, &user)
	if err != nil {
		log.Error().Msgf("", err)
	}

	res.Correct = pass == user.Password

	log.Trace().Msgf("CheckUser %d", res.Correct)

	return res, nil
}

// loadUsers loads hotel users from mongodb.
func loadUsers(client *mongo.Client) map[string]string {
	collection := client.Database("user-db").Collection("user")
	curr, err := collection.Find(context.TODO(), bson.D{})
	if err != nil {
		log.Error().Msgf("Failed get users data: ", err)
	}

	var users []User
	curr.All(context.TODO(), &users)
	if err != nil {
		log.Error().Msgf("Failed get users data: ", err)
	}

	res := make(map[string]string)
	for _, user := range users {
		res[user.Username] = user.Password
	}

	log.Trace().Msg("Done load users")

	return res
}

type User struct {
	Username string `bson:"username"`
	Password string `bson:"password"`
}
