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
	"sort"
	"sync"
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

	grpc_zap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
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
	Logger       *zap.Logger
	LikesLatency *prometheus.HistogramVec
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
		s.Logger.Info("Redis start to check cached posts")
		posts, err := rdb.Get(rctx, "cached_posts").Bytes()
		s.Logger.Info("Redis ended to check cached posts")

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

	var wg sync.WaitGroup
	mutex := &sync.Mutex{}
	errs := make(chan error, len(posts))

	for _, post := range posts {
		wg.Add(1)
		go func(post models.Post, logger *zap.Logger) {
			defer wg.Done()
			logger.Info("Redis: start get total likes;", zap.Uint("post_id", post.ID))
			start := time.Now()
			likes, err := rdb.SCard(rctx, "post_"+string(post.ID)).Result()
			s.LikesLatency.WithLabelValues("likes_latency").Observe(float64(time.Since(start).Milliseconds()))
			logger.Info("Redis: ended get total likes;", zap.Uint("post_id", post.ID))

			if err != nil {
				errs <- err
				return
			}

			logger.Info("Redis: start get is_liked;", zap.Uint("post_id", post.ID), zap.Int64("user_id", req.UserId))
			is_liked, err := rdb.SIsMember(rctx, "post_"+string(post.ID), req.UserId).Result()
			logger.Info("Redis: ended get is_liked;", zap.Uint("post_id", post.ID), zap.Int64("user_id", req.UserId))

			if err != nil {
				errs <- err
				return
			}

			mutex.Lock()
			defer mutex.Unlock()
			posts_rsp = append(posts_rsp,
				&api.Post{
					Id:       int64(post.ID),
					Post:     &api.PostBody{Title: post.Title, Body: post.Body},
					Author:   &api.UserInfo{Id: int64(post.Author.ID), Name: post.Author.Name},
					Likes:    likes,
					IsLiked:  is_liked,
					Comments: int64(len(post.Comments)),
				})
		}(post, s.Logger)
	}

	wg.Wait()
	close(errs)

	if err := <-errs; err != nil {
		return &api.GetPostsRsp{}, status.Error(codes.Internal, err.Error())
	}

	sort.Slice(posts_rsp, func(i, j int) bool {
		return posts_rsp[i].Id < posts_rsp[j].Id
	})

	if !request_main_page {
		return &api.GetPostsRsp{Posts: posts_rsp}, nil
	}

	// save cache to redis
	posts_to_cache, err := json.Marshal(posts_rsp)
	if err != nil {
		return &api.GetPostsRsp{}, status.Error(codes.Internal, err.Error())
	}
	s.Logger.Info("Redis start to save cached posts")
	err = rdb.Set(rctx, "cached_posts", posts_to_cache, 5*time.Second).Err()
	s.Logger.Info("Redis ended to save cached posts")
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

	s.Logger.Info("Redis: start get total likes;", zap.Uint("post_id", post.ID))
	start := time.Now()
	likes, err := rdb.SCard(rctx, "post_"+string(post.ID)).Result()
	s.LikesLatency.WithLabelValues("likes_latency").Observe(float64(time.Since(start).Milliseconds()))
	s.Logger.Info("Redis: ended get total likes;", zap.Uint("post_id", post.ID))
	if err != nil {
		return &api.EditPostRsp{}, status.Error(codes.Internal, err.Error())
	}

	s.Logger.Info("Redis: start get is_liked;", zap.Uint("post_id", post.ID))
	is_liked, err := rdb.SIsMember(rctx, "post_"+string(post.ID), req.UserId).Result()
	s.Logger.Info("Redis: ended get is_liked;", zap.Uint("post_id", post.ID))
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

	s.Logger.Info("Redis: start delete all likes;", zap.Uint("post_id", post.ID))
	_, err := rdb.Del(rctx, "post_"+string(post.ID)).Result()
	s.Logger.Info("Redis: ended delete all likes;", zap.Uint("post_id", post.ID))
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

	s.Logger.Info("Redis: start add like;", zap.Int64("post_id", req.PostId), zap.Int64("user_id", req.UserId))
	liked, err := rdb.SAdd(rctx, "post_"+string(req.PostId), req.UserId).Result()
	s.Logger.Info("Redis: ended add like;", zap.Int64("post_id", req.PostId), zap.Int64("user_id", req.UserId))
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

	s.Logger.Info("Redis: start delete like;", zap.Int64("post_id", req.PostId), zap.Int64("user_id", req.UserId))
	disliked, err := rdb.SRem(rctx, "post_"+string(req.PostId), req.UserId).Result()
	s.Logger.Info("Redis: ended delete like;", zap.Int64("post_id", req.PostId), zap.Int64("user_id", req.UserId))
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

	var wg sync.WaitGroup
	mutex := &sync.Mutex{}
	errs := make(chan error, len(comments))

	for _, comment := range comments {
		wg.Add(1)
		go func(comment models.Comment, logger *zap.Logger) {
			defer wg.Done()
			logger.Info("Redis: start get likes;", zap.Uint("comment_id", comment.ID))
			likes, err := rdb.SCard(rctx, "comment_"+string(comment.ID)).Result()
			logger.Info("Redis: ended get likes;", zap.Uint("comment_id", comment.ID))
			if err != nil {
				errs <- err
				return
			}

			logger.Info("Redis: start get is_liked;", zap.Uint("comment_id", comment.ID), zap.Int64("user_id", req.UserId))
			is_liked, err := rdb.SIsMember(rctx, "comment_"+string(comment.ID), req.UserId).Result()
			logger.Info("Redis: ended get is_liked;", zap.Uint("comment_id", comment.ID), zap.Int64("user_id", req.UserId))
			if err != nil {
				errs <- err
				return
			}

			mutex.Lock()
			defer mutex.Unlock()
			comments_rsp = append(comments_rsp,
				&api.Comment{
					Id:      int64(comment.ID),
					PostId:  int64(comment.PostRefer),
					Author:  &api.UserInfo{Id: int64(comment.Author.ID), Name: comment.Author.Name},
					Body:    comment.Body,
					Likes:   likes,
					IsLiked: is_liked,
				})
		}(comment, s.Logger)
	}

	wg.Wait()
	close(errs)

	if err := <-errs; err != nil {
		return &api.GetCommentsRsp{}, status.Error(codes.Internal, err.Error())
	}

	sort.Slice(comments_rsp, func(i, j int) bool {
		return comments_rsp[i].Id < comments_rsp[j].Id
	})

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

	s.Logger.Info("Redis: start get total likes;", zap.Uint("comment_id", comment.ID))
	likes, err := rdb.SCard(rctx, "comment_"+string(comment.ID)).Result()
	s.Logger.Info("Redis: ended get total likes;", zap.Uint("comment_id", comment.ID))
	if err != nil {
		return &api.EditCommentRsp{}, status.Error(codes.Internal, err.Error())
	}

	s.Logger.Info("Redis: start get is_liked;", zap.Uint("comment_id", comment.ID), zap.Int64("user_id", req.UserId))
	is_liked, err := rdb.SIsMember(rctx, "comment_"+string(comment.ID), req.UserId).Result()
	s.Logger.Info("Redis: ended get is_liked;", zap.Uint("comment_id", comment.ID), zap.Int64("user_id", req.UserId))
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

	s.Logger.Info("Redis: start delete all likes;", zap.Uint("comment_id", comment.ID))
	_, err := rdb.Del(rctx, "comment_"+string(comment.ID)).Result()
	s.Logger.Info("Redis: ended delete all likes;", zap.Uint("comment_id", comment.ID))
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

	s.Logger.Info("Redis: start add like;", zap.Int64("comment_id", req.CommentId), zap.Int64("user_id", req.UserId))
	liked, err := rdb.SAdd(rctx, "comment_"+string(req.CommentId), req.UserId).Result()
	s.Logger.Info("Redis: ended add like;", zap.Int64("comment_id", req.CommentId), zap.Int64("user_id", req.UserId))
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

	s.Logger.Info("Redis: start delete like;", zap.Int64("comment_id", req.CommentId), zap.Int64("user_id", req.UserId))
	disliked, err := rdb.SRem(rctx, "comment_"+string(req.CommentId), req.UserId).Result()
	s.Logger.Info("Redis: ended delete like;", zap.Int64("comment_id", req.CommentId), zap.Int64("user_id", req.UserId))
	if err != nil {
		return &api.DislikeCommentRsp{}, status.Error(codes.Internal, err.Error())
	}

	if disliked == 0 {
		return &api.DislikeCommentRsp{}, status.Error(codes.AlreadyExists, "You already disliked this comment!")
	}

	return &api.DislikeCommentRsp{}, nil
}

func connectDB() {
	postgresHost, exists := os.LookupEnv("POSTGRES_HOST")
	if !exists {
		panic("postgresHost is not set")
	}

	postgresUser, exists := os.LookupEnv("POSTGRES_USER")
	if !exists {
		panic("postgresUser is not set")
	}

	postgresPassword, exists := os.LookupEnv("POSTGRES_PASSWORD")
	if !exists {
		panic("postgresPassword is not set")
	}

	postgresPort, exists := os.LookupEnv("POSTGRES_PORT")
	if !exists {
		panic("postgresPort is not set")
	}

	docker_url := "host=" + postgresHost +
		" user=" + postgresUser +
		" password=" + postgresPassword +
		" port=" + postgresPort +
		" sslmode=disable TimeZone=UTC"

	fmt.Printf("Docker URL: %s\n", docker_url)

	var err error
	db, err = gorm.Open(postgres.Open(
		docker_url,
	), &gorm.Config{})
	if err != nil {
		panic(err)
	}

	err = db.AutoMigrate(&models.User{}, &models.Post{}, &models.Comment{})
	if err != nil {
		panic(err)
	}
}

func connectRedis() {
	redisURL, exists := os.LookupEnv("REDIS_URL")
	if !exists {
		panic("REDIS_URL is not set")
	}

	rctx = context.Background()
	rdb = redis.NewClient(&redis.Options{Addr: redisURL})
	// rdb = redis.NewClient(&redis.Options{Addr: "89.169.8.65:6379", Password: os.Getenv("REDIS_PASSWORD"), DB: 0})
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
	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	connectDB()
	connectRedis()
	fillDBIfEmpty()

	// just to test DB
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

	// Для анализа latency запросов:
	// 1. Counter плохо подходит. Можно завести два счётчика - сумму latency и
	//    количество запросов, чтобы считать среднее latency, но это малоинформативно.
	// 2. Gauge не подходит. Можно разве что выводить последнее значение latency.
	// 3. Histogram подходит отлично, наглядно видно распределение latency. При этом
	//    достаточно просто вычислется - инкрементом соответствующего бакета.
	// 4. Summary подходит, но по идее будет сопровождаться проблемами производительности,
	//    так как необходимо хранить весь массив данных и сортировать.
	//
	// Также умные люди замечают, что расчёт квантилей происходит на стороне клинета,
	// а уже готовые квантили с разными лейблами мы не сможем агрегировать :(
	// https://chronosphere.io/learn/an-introduction-to-the-four-primary-types-of-prometheus-metrics/
	//
	// Поэтому выбираем Histogram.
	likes_latency := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "likes_latency",
			Buckets: []float64{0, 20, 40, 60, 80, 100, 120, 140, 160, 180, 200},
		},
		[]string{"likes_latency"},
	)
	prometheus.MustRegister(likes_latency)

	go func() {
		server := &http.Server{
			Addr:    ":9090",
			Handler: promhttp.Handler(),
		}

		log.Println("Serving metrics on http://0.0.0.0:9090")
		log.Fatalln(server.ListenAndServe())
	}()

	// Start gRPC server
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	s := &Service{
		Logger:       logger,
		LikesLatency: likes_latency,
	}

	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			grpc_zap.UnaryServerInterceptor(logger),
			grpc_prometheus.UnaryServerInterceptor,
		),
	)
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
