package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"slices"

	api "go_1C/api"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"golang.org/x/exp/rand"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

var rnd = rand.New(rand.NewSource(99))

var posts = make([]*api.Post, 0)
var users = make([]*api.UserInfo, 0)

// post_id -> [user_ids]
var likes = make(map[int64][]int64)

type Service struct {
	api.UnimplementedServiceServer
}

func (s *Service) GetPosts(
	ctx context.Context, req *api.GetPostsReq,
) (*api.GetPostsRsp, error) {
	log.Println("User:", req.UserId, "callded GetPosts")

	l := max(0, req.Offset)
	r := min(len(posts), int(req.Offset+req.Limit))
	posts_rsp := posts[l:r]
	for _, post := range posts_rsp {
		post.Likes = int64(len(likes[post.Id]))
		post.IsLiked = slices.Contains(likes[post.Id], req.UserId)
	}

	return &api.GetPostsRsp{Posts: posts_rsp}, nil
}

func (s *Service) CreatePost(
	ctx context.Context, req *api.CreatePostReq,
) (*api.CreatePostRsp, error) {
	log.Println("User:", req.UserId, "callded CreatePost")

	posts = append(posts, &api.Post{Id: posts[len(posts)-1].Id + 1, Post: req.Post, Author: users[req.UserId]})
	return &api.CreatePostRsp{Post: posts[len(posts)-1]}, nil
}

func (s *Service) EditPost(
	ctx context.Context, req *api.EditPostReq,
) (*api.EditPostRsp, error) {
	log.Println("User:", req.UserId, "callded EditPost")

	for _, post := range posts {
		if post.Id == req.PostId {
			if post.Author.Id != req.UserId {
				return &api.EditPostRsp{}, status.Error(1, "You are not the author!")
			}
			post.Post = req.Post
			return &api.EditPostRsp{}, nil
		}
	}

	return &api.EditPostRsp{}, status.Error(1, "Post not found")
}

func DeletePost(ctx context.Context, req *api.DeletePostReq) (*api.DeletePostRsp, error) {
	log.Println("User:", req.UserId, "callded DeletePost")

	for _, post := range posts {
		if post.Id == req.PostId {
			if post.Author.Id != req.UserId {
				return &api.DeletePostRsp{}, status.Error(1, "You are not the author!")
			}
			posts = slices.DeleteFunc(posts, func(p *api.Post) bool {
				return post.Id == p.Id
			})
			return &api.DeletePostRsp{}, nil
		}
	}

	return &api.DeletePostRsp{}, status.Error(1, "Post not found")
}

func LikePost(ctx context.Context, req *api.LikePostReq) (*api.LikePostRsp, error) {
	log.Println("User:", req.UserId, "callded LikePost")
	if slices.Contains(likes[req.PostId], req.UserId) {
		return &api.LikePostRsp{}, status.Error(1, "Already liked")
	}

	likes[req.PostId] = append(likes[req.PostId], req.UserId)
	return &api.LikePostRsp{}, nil
}

func DislikePost(ctx context.Context, req *api.DislikePostReq) (*api.DislikePostRsp, error) {
	log.Println("User:", req.UserId, "callded DislikePost")

	likes[req.PostId] = slices.DeleteFunc(likes[req.PostId], func(id int64) bool {
		return id == req.UserId
	})
	return &api.DislikePostRsp{}, nil
}

func GetComments(ctx context.Context, req *api.GetCommentsReq) (*api.GetCommentsRsp, error) {
	log.Println("User:", req.UserId, "callded GetComments")
	return &api.GetCommentsRsp{}, nil
}

func CreateComment(ctx context.Context, req *api.CreateCommentReq) (*api.CreateCommentRsp, error) {
	log.Println("User:", req.UserId, "callded CreateComment")
	return &api.CreateCommentRsp{}, nil
}

func EditComment(ctx context.Context, req *api.EditCommentReq) (*api.EditCommentRsp, error) {
	log.Println("User:", req.UserId, "callded EditComment")
	return &api.EditCommentRsp{}, nil
}

func DeleteComment(ctx context.Context, req *api.DeleteCommentReq) (*api.DeleteCommentRsp, error) {
	log.Println("User:", req.UserId, "callded DeleteComment")
	return &api.DeleteCommentRsp{}, nil
}

func LikeComment(ctx context.Context, req *api.LikeCommentReq) (*api.LikeCommentRsp, error) {
	log.Println("User:", req.UserId, "callded LikeComment")
	return &api.LikeCommentRsp{}, nil
}

func DislikeComment(ctx context.Context, req *api.DislikeCommentReq) (*api.DislikeCommentRsp, error) {
	log.Println("User:", req.UserId, "callded DislikeComment")
	return &api.DislikeCommentRsp{}, nil
}

func genMockData() {
	for i := int64(0); i < 10; i++ {
		users = append(users, &api.UserInfo{Id: i, Name: "User " + fmt.Sprint(i)})
	}

	for i := int64(0); i < 15; i++ {
		posts = append(posts,
			&api.Post{Id: int64(i), Post: &api.PostBody{Title: "Post " + fmt.Sprint(i), Body: "Body " + fmt.Sprint(i)}, Author: users[rand.Intn(len(users))]},
		)
		for k := 0; k < rnd.Intn(10); k++ {
			likes[i] = append(likes[i], rnd.Int63n(10))
		}
		slices.Sort(likes[i])
		likes[i] = slices.Compact(likes[i])
	}
}

//go:embed api/api.swagger.json
var swaggerData []byte

//go:embed swagger-ui
var swaggerFiles embed.FS

func main() {
	genMockData()

	// Start gRPC server
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	s := &Service{}

	grpcServer := grpc.NewServer()
	api.RegisterServiceServer(grpcServer, s)
	reflection.Register(grpcServer)

	go func() {
		log.Println("gRPC server started on :50051")
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("Failed to serve: %v", err)
		}
	}()

	conn, err := grpc.NewClient(":50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalln("Failed to dial server:", err)
	}

	gwmux := runtime.NewServeMux()

	ctx := context.Background()
	if err := api.RegisterServiceHandler(ctx, gwmux, conn); err != nil {
		log.Fatalln("Failed to register gateway:", err)
	}

	mux := http.NewServeMux()

	mux.Handle("/", gwmux)

	mux.HandleFunc("/swagger-ui/swagger.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(swaggerData)
	})

	fSys, err := fs.Sub(swaggerFiles, "swagger-ui")
	if err != nil {
		panic(err)
	}

	mux.Handle("/swagger-ui/", http.StripPrefix("/swagger-ui/", http.FileServer(http.FS(fSys))))

	gwServer := &http.Server{
		Addr:    ":8090",
		Handler: mux,
	}

	log.Println("Serving gRPC-Gateway on http://0.0.0.0:8090")
	log.Fatalln(gwServer.ListenAndServe())
}
