package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/redis/go-redis/v9"

	api "go_1C/api"
	"go_1C/models"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

//go:embed api/api.swagger.json
var swaggerData []byte

//go:embed swagger-ui
var swaggerFiles embed.FS

var db *gorm.DB
var rdb *redis.Client
var rctx context.Context

const cached_posts_limit int64 = 30

type Service struct {
	api.UnimplementedServiceServer
}

func (s *Service) GetPosts(
	ctx context.Context, req *api.GetPostsReq,
) (*api.GetPostsRsp, error) {
	log.Println("User:", req.UserId, "callded GetPosts")

	if req.Offset < 0 || req.Limit < 1 {
		return &api.GetPostsRsp{}, status.Error(codes.Internal, "Invalid offset or limit!")
	}

	var request_main_page = req.Offset+req.Limit < cached_posts_limit && req.UserId == 0
	var posts_rsp []*api.Post

	if request_main_page {
		// may be cached
		posts, err := rdb.Get(rctx, "cached_posts").Bytes()
		if err != redis.Nil && err != nil {
			return &api.GetPostsRsp{}, status.Error(codes.Internal, err.Error())
		} else if err != redis.Nil {
			err = json.Unmarshal(posts, &posts_rsp)
			if err != nil {
				return &api.GetPostsRsp{}, status.Error(codes.Internal, err.Error())
			}

			fmt.Println("CACHED:", posts_rsp)

			return &api.GetPostsRsp{Posts: posts_rsp[req.Offset:min(req.Offset+req.Limit, int64(len(posts_rsp)))]}, nil
		} else {
			// not cached
			// falthrough
		}
	}

	var posts []models.Post
	if request_main_page {
		if err := db.Preload("Author").Preload("Comments").Offset(0).Limit(int(cached_posts_limit)).Find(&posts).Error; err != nil {
			return &api.GetPostsRsp{}, status.Error(codes.Internal, err.Error())
		}
	} else {
		if err := db.Preload("Author").Preload("Comments").Offset(int(req.Offset)).Limit(int(req.Limit)).Find(&posts).Error; err != nil {
			return &api.GetPostsRsp{}, status.Error(codes.Internal, err.Error())
		}
	}

	for _, post := range posts {
		likes, err := rdb.SCard(rctx, "post_"+string(post.ID)).Result()
		if err != nil {
			return &api.GetPostsRsp{}, status.Error(codes.Internal, err.Error())
		}

		is_liked, err := rdb.SIsMember(rctx, "post_"+string(post.ID), req.UserId).Result()
		if err != nil {
			return &api.GetPostsRsp{}, status.Error(codes.Internal, err.Error())
		}

		posts_rsp = append(posts_rsp,
			&api.Post{
				Id:       int64(post.ID),
				Post:     &api.PostBody{Title: post.Title, Body: post.Body},
				Author:   &api.UserInfo{Id: int64(post.Author.ID), Name: post.Author.Name},
				Likes:    likes,
				IsLiked:  is_liked,
				Comments: int64(len(post.Comments)),
			})
	}

	if !request_main_page {
		return &api.GetPostsRsp{Posts: posts_rsp}, nil
	}

	// save cache to redis
	posts_to_cache, err := json.Marshal(posts_rsp)
	if err != nil {
		return &api.GetPostsRsp{}, status.Error(codes.Internal, err.Error())
	}
	err = rdb.Set(rctx, "cached_posts", posts_to_cache, 5*time.Second).Err()
	if err != nil {
		return &api.GetPostsRsp{}, status.Error(codes.Internal, err.Error())
	}

	return &api.GetPostsRsp{Posts: posts_rsp[req.Offset:min(req.Offset+req.Limit, int64(len(posts_rsp)))]}, nil
}

func (s *Service) CreatePost(
	ctx context.Context, req *api.CreatePostReq,
) (*api.CreatePostRsp, error) {
	log.Println("User:", req.UserId, "callded CreatePost")

	new_post := &models.Post{Title: req.Post.Title, Body: req.Post.Body, AuthorID: uint(req.UserId)}

	if err := db.Preload("Author").Create(&new_post).First(&new_post).Error; err != nil {
		return &api.CreatePostRsp{}, status.Error(codes.Internal, err.Error())
	}

	return &api.CreatePostRsp{
		Post: &api.Post{
			Id:       int64(new_post.ID),
			Post:     &api.PostBody{Title: new_post.Title, Body: new_post.Body},
			Author:   &api.UserInfo{Id: int64(new_post.Author.ID), Name: new_post.Author.Name},
			Likes:    0,
			IsLiked:  false,
			Comments: 0,
		}}, nil
}

func (s *Service) EditPost(
	ctx context.Context, req *api.EditPostReq,
) (*api.EditPostRsp, error) {
	log.Println("User:", req.UserId, "callded EditPost")

	var post *models.Post
	if err := db.Where("ID = ?", req.PostId).Preload("Author").Preload("Comments").First(&post).Error; err != nil {
		return &api.EditPostRsp{}, status.Error(codes.Internal, err.Error())
	}

	if post.AuthorID != uint(req.UserId) {
		return &api.EditPostRsp{}, status.Error(codes.Unauthenticated, "You are not the author!")
	}

	post.Title = req.Post.Title
	post.Body = req.Post.Body

	if err := db.Save(&post).Error; err != nil {
		return &api.EditPostRsp{}, status.Error(codes.Internal, err.Error())
	}

	likes, err := rdb.SCard(rctx, "post_"+string(post.ID)).Result()
	if err != nil {
		return &api.EditPostRsp{}, status.Error(codes.Internal, err.Error())
	}

	is_liked, err := rdb.SIsMember(rctx, "post_"+string(post.ID), req.UserId).Result()
	if err != nil {
		return &api.EditPostRsp{}, status.Error(codes.Internal, err.Error())
	}

	return &api.EditPostRsp{
		Post: &api.Post{
			Id:       int64(post.ID),
			Post:     &api.PostBody{Title: post.Title, Body: post.Body},
			Author:   &api.UserInfo{Id: int64(post.Author.ID), Name: post.Author.Name},
			Likes:    likes,
			IsLiked:  is_liked,
			Comments: int64(len(post.Comments)),
		},
	}, nil
}

func (s *Service) DeletePost(ctx context.Context, req *api.DeletePostReq) (*api.DeletePostRsp, error) {
	log.Println("User:", req.UserId, "callded DeletePost")

	var post *models.Post
	if err := db.Where("ID = ?", req.PostId).First(&post).Error; err != nil {
		return &api.DeletePostRsp{}, status.Error(codes.Internal, err.Error())
	}

	if post.AuthorID != uint(req.UserId) {
		return &api.DeletePostRsp{}, status.Error(codes.Unauthenticated, "You are not the author!")
	}

	if err := db.Delete(&post).Error; err != nil {
		return &api.DeletePostRsp{}, status.Error(codes.Internal, err.Error())
	}

	_, err := rdb.Del(rctx, "post_"+string(post.ID)).Result()
	if err != nil {
		return &api.DeletePostRsp{}, status.Error(codes.Internal, err.Error())
	}

	return &api.DeletePostRsp{}, nil
}

func (s *Service) LikePost(ctx context.Context, req *api.LikePostReq) (*api.LikePostRsp, error) {
	log.Println("User:", req.UserId, "callded LikePost")

	if req.UserId == 0 {
		return &api.LikePostRsp{}, status.Error(codes.Unauthenticated, "You are not logged in!")
	}

	liked, err := rdb.SAdd(rctx, "post_"+string(req.PostId), req.UserId).Result()
	if err != nil {
		return &api.LikePostRsp{}, status.Error(codes.Internal, err.Error())
	}

	if liked == 0 {
		return &api.LikePostRsp{}, status.Error(codes.AlreadyExists, "You already liked this post!")
	}

	return &api.LikePostRsp{}, nil
}

func (s *Service) DislikePost(ctx context.Context, req *api.DislikePostReq) (*api.DislikePostRsp, error) {
	log.Println("User:", req.UserId, "callded DislikePost")

	if req.UserId == 0 {
		return &api.DislikePostRsp{}, status.Error(codes.Unauthenticated, "You are not logged in!")
	}

	disliked, err := rdb.SRem(rctx, "post_"+string(req.PostId), req.UserId).Result()
	if err != nil {
		return &api.DislikePostRsp{}, status.Error(codes.Internal, err.Error())
	}

	if disliked == 0 {
		return &api.DislikePostRsp{}, status.Error(codes.AlreadyExists, "You already disliked this post!")
	}

	return &api.DislikePostRsp{}, nil
}

func (s *Service) GetComments(ctx context.Context, req *api.GetCommentsReq) (*api.GetCommentsRsp, error) {
	log.Println("User:", req.UserId, "callded GetComments")

	var comments []models.Comment
	if err := db.Where("post_refer = ?", req.PostId).Offset(int(req.Offset)).Limit(int(req.Limit)).Preload("Author").Find(&comments).Error; err != nil {
		return &api.GetCommentsRsp{}, status.Error(codes.Internal, err.Error())
	}

	var comments_rsp []*api.Comment
	for _, comment := range comments {
		likes, err := rdb.SCard(rctx, "comment_"+string(comment.ID)).Result()
		if err != nil {
			return &api.GetCommentsRsp{}, status.Error(codes.Internal, err.Error())
		}

		is_liked, err := rdb.SIsMember(rctx, "comment_"+string(comment.ID), req.UserId).Result()
		if err != nil {
			return &api.GetCommentsRsp{}, status.Error(codes.Internal, err.Error())
		}

		comments_rsp = append(comments_rsp,
			&api.Comment{
				Id:      int64(comment.ID),
				PostId:  int64(comment.PostRefer),
				Author:  &api.UserInfo{Id: int64(comment.Author.ID), Name: comment.Author.Name},
				Body:    comment.Body,
				Likes:   likes,
				IsLiked: is_liked,
			})
	}

	return &api.GetCommentsRsp{Comments: comments_rsp}, nil
}

func (s *Service) CreateComment(ctx context.Context, req *api.CreateCommentReq) (*api.CreateCommentRsp, error) {
	log.Println("User:", req.UserId, "callded CreateComment")

	new_comment := &models.Comment{PostRefer: uint(req.PostId), AuthorID: uint(req.UserId), Body: req.Body}

	if err := db.Preload("Author").Create(&new_comment).First(&new_comment).Error; err != nil {
		return &api.CreateCommentRsp{}, status.Error(codes.Internal, err.Error())
	}

	return &api.CreateCommentRsp{
		Comment: &api.Comment{
			Id:      int64(new_comment.ID),
			PostId:  int64(new_comment.PostRefer),
			Author:  &api.UserInfo{Id: int64(new_comment.Author.ID), Name: new_comment.Author.Name},
			Body:    new_comment.Body,
			Likes:   0,
			IsLiked: false,
		},
	}, nil
}

func (s *Service) EditComment(ctx context.Context, req *api.EditCommentReq) (*api.EditCommentRsp, error) {
	log.Println("User:", req.UserId, "callded EditComment")

	var comment *models.Comment
	if err := db.Where("ID = ?", req.CommentId).Preload("Author").First(&comment).Error; err != nil {
		return &api.EditCommentRsp{}, status.Error(codes.Internal, err.Error())
	}

	if comment.AuthorID != uint(req.UserId) {
		return &api.EditCommentRsp{}, status.Error(codes.Unauthenticated, "You are not the author!")
	}

	comment.Body = req.Body

	if err := db.Save(&comment).Error; err != nil {
		return &api.EditCommentRsp{}, status.Error(codes.Internal, err.Error())
	}

	likes, err := rdb.SCard(rctx, "comment_"+string(comment.ID)).Result()
	if err != nil {
		return &api.EditCommentRsp{}, status.Error(codes.Internal, err.Error())
	}

	is_liked, err := rdb.SIsMember(rctx, "comment_"+string(comment.ID), req.UserId).Result()
	if err != nil {
		return &api.EditCommentRsp{}, status.Error(codes.Internal, err.Error())
	}

	return &api.EditCommentRsp{
		Comment: &api.Comment{
			Id:      int64(comment.ID),
			PostId:  int64(comment.PostRefer),
			Author:  &api.UserInfo{Id: int64(comment.Author.ID), Name: comment.Author.Name},
			Body:    comment.Body,
			Likes:   likes,
			IsLiked: is_liked,
		},
	}, nil
}

func (s *Service) DeleteComment(ctx context.Context, req *api.DeleteCommentReq) (*api.DeleteCommentRsp, error) {
	log.Println("User:", req.UserId, "callded DeleteComment")

	var comment *models.Comment
	if err := db.Where("ID = ?", req.CommentId).First(&comment).Error; err != nil {
		return &api.DeleteCommentRsp{}, status.Error(codes.Internal, err.Error())
	}

	if comment.AuthorID != uint(req.UserId) {
		return &api.DeleteCommentRsp{}, status.Error(codes.Unauthenticated, "You are not the author!")
	}

	if err := db.Delete(&comment).Error; err != nil {
		return &api.DeleteCommentRsp{}, status.Error(codes.Internal, err.Error())
	}

	_, err := rdb.Del(rctx, "comment_"+string(comment.ID)).Result()
	if err != nil {
		return &api.DeleteCommentRsp{}, status.Error(codes.Internal, err.Error())
	}

	return &api.DeleteCommentRsp{}, nil
}

func (s *Service) LikeComment(ctx context.Context, req *api.LikeCommentReq) (*api.LikeCommentRsp, error) {
	log.Println("User:", req.UserId, "callded LikeComment")

	if req.UserId == 0 {
		return &api.LikeCommentRsp{}, status.Error(codes.Unauthenticated, "You are not logged in!")
	}

	liked, err := rdb.SAdd(rctx, "comment_"+string(req.CommentId), req.UserId).Result()
	if err != nil {
		return &api.LikeCommentRsp{}, status.Error(codes.Internal, err.Error())
	}

	if liked == 0 {
		return &api.LikeCommentRsp{}, status.Error(codes.AlreadyExists, "You already liked this comment!")
	}

	return &api.LikeCommentRsp{}, nil
}

func (s *Service) DislikeComment(ctx context.Context, req *api.DislikeCommentReq) (*api.DislikeCommentRsp, error) {
	log.Println("User:", req.UserId, "callded DislikeComment")

	if req.UserId == 0 {
		return &api.DislikeCommentRsp{}, status.Error(codes.Unauthenticated, "You are not logged in!")
	}

	disliked, err := rdb.SRem(rctx, "comment_"+string(req.CommentId), req.UserId).Result()
	if err != nil {
		return &api.DislikeCommentRsp{}, status.Error(codes.Internal, err.Error())
	}

	if disliked == 0 {
		return &api.DislikeCommentRsp{}, status.Error(codes.AlreadyExists, "You already disliked this comment!")
	}

	return &api.DislikeCommentRsp{}, nil
}

func connectDB() {
	var err error
	db, err = gorm.Open(postgres.Open("host=localhost dbname=postgres port=5432 sslmode=disable TimeZone=UTC"), &gorm.Config{})
	if err != nil {
		panic(err)
	}

	err = db.AutoMigrate(&models.User{}, &models.Post{}, &models.Comment{})
	if err != nil {
		panic(err)
	}
}

func connectRedis() {
	rctx = context.Background()
	rdb = redis.NewClient(&redis.Options{Addr: "89.169.8.65:6379", Password: os.Getenv("REDIS_PASSWORD"), DB: 0})
}

func fillDBIfEmpty() {
	if db == nil {
		panic("DB is not connected")
	}

	var count int64
	db.Model(&models.User{}).Count(&count)
	if count > 0 {
		return
	}

	// Create sample Users
	users := []models.User{
		{Name: "User is not logged in"},
		{Name: "Bob Johnson"},
		{Name: "Charlie Davis"},
		{Name: "Alex Rusin"},
	}

	if err := db.Create(&users).Error; err != nil {
		log.Fatalf("Failed to insert users: %v", err)
	}

	// Create sample Posts
	initOrders := []models.Post{
		{Title: "Title 1", Body: "Body 1", AuthorID: 1},
		{Title: "Title 2", Body: "Body 2", AuthorID: 2},
		{Title: "Title 3", Body: "Body 3", AuthorID: 2, Comments: []models.Comment{{Body: "Comment 1", AuthorID: 1}, {Body: "Comment 2", AuthorID: 3}}},
		{Title: "Title 4", Body: "Body 4", AuthorID: 3, Comments: []models.Comment{{Body: "Comment 3", AuthorID: 1}}},
	}

	if err := db.Create(&initOrders).Error; err != nil {
		log.Fatalf("Failed to insert orders: %v", err)
	}

	fmt.Println("Данные созданы")
}

func main() {
	connectDB()
	connectRedis()
	fillDBIfEmpty()

	var posts []models.Post
	db.Preload("Author").Preload("Comments").Find(&posts)

	for _, post := range posts {
		fmt.Println(post)
	}

	var comments []models.Comment
	db.Preload("Author").Find(&comments)

	for _, comment := range comments {
		fmt.Println(comment)
	}

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
