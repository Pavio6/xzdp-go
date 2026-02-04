package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"hmdp-backend/internal/model"
	"hmdp-backend/internal/utils"
)

// BlogService 处理博客相关业务逻辑
type BlogService struct {
	db        *gorm.DB
	rdb       *redis.Client
	followSvc *FollowService
}

// NewBlogService 创建 BlogService 实例
func NewBlogService(db *gorm.DB, rdb *redis.Client, followSvc *FollowService) *BlogService {
	return &BlogService{db: db, rdb: rdb, followSvc: followSvc}
}

func (s *BlogService) Create(ctx context.Context, blog *model.Blog) error {
	if err := s.db.WithContext(ctx).Create(blog).Error; err != nil {
		return err
	}
	// 推模式：将新笔记推送到粉丝的收件箱（ZSet，score 为时间戳，越新越靠前）
	if s.followSvc != nil {
		fans, err := s.followSvc.FollowerIDs(ctx, blog.UserID)
		if err != nil {
			return err
		}
		score := float64(time.Now().UnixMilli())
		for _, fan := range fans {
			key := fmt.Sprintf("%s%d", utils.FEED_KEY, fan)
			_ = s.rdb.ZAdd(ctx, key, redis.Z{Score: score, Member: blog.ID}).Err()
		}
	}
	return nil
}

func (s *BlogService) GetByID(ctx context.Context, id int64) (*model.Blog, error) {
	var blog model.Blog
	err := s.db.WithContext(ctx).First(&blog, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &blog, nil
}

func (s *BlogService) IncrementLike(ctx context.Context, id int64) error {
	return s.db.WithContext(ctx).
		Model(&model.Blog{}).
		Where("id = ?", id).
		UpdateColumn("liked", gorm.Expr("liked + 1")).
		Error
}

func (s *BlogService) QueryByUser(ctx context.Context, userID int64, page, size int) ([]model.Blog, error) {
	var blogs []model.Blog
	offset := (page - 1) * size
	if offset < 0 {
		offset = 0
	}
	err := s.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("id ASC").
		Offset(offset).
		Limit(size).
		Find(&blogs).Error
	return blogs, err
}

func (s *BlogService) QueryHot(ctx context.Context, page, size int) ([]model.Blog, error) {
	var blogs []model.Blog
	offset := (page - 1) * size
	if offset < 0 {
		offset = 0
	}
	err := s.db.WithContext(ctx).
		Order("liked DESC").
		Offset(offset).
		Limit(size).
		Find(&blogs).Error
	return blogs, err
}

// ToggleLike 点赞/取消点赞；返回 true 表示点赞后状态
func (s *BlogService) ToggleLike(ctx context.Context, blogID, userID int64) (bool, error) {
	key := fmt.Sprintf("%s%d", utils.BLOG_LIKED_KEY, blogID)
	// 判断当前用户是否已点赞（使用 ZSet）
	_, err := s.rdb.ZScore(ctx, key, fmt.Sprint(userID)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, err
	}
	// 点赞流程
	if errors.Is(err, redis.Nil) {
		if err := s.db.WithContext(ctx).
			Model(&model.Blog{}).
			Where("id = ?", blogID).
			UpdateColumn("liked", gorm.Expr("liked + 1")).Error; err != nil {
			return false, err
		}
		if err := s.rdb.ZAdd(ctx, key, redis.Z{
			Score:  float64(time.Now().Unix()),
			Member: fmt.Sprint(userID),
		}).Err(); err != nil {
			return false, err
		}
		return true, nil
	}

	// 取消点赞
	if err := s.db.WithContext(ctx).
		Model(&model.Blog{}).
		Where("id = ? AND liked > 0", blogID).
		UpdateColumn("liked", gorm.Expr("liked - 1")).Error; err != nil {
		return false, err
	}
	if err := s.rdb.ZRem(ctx, key, fmt.Sprint(userID)).Err(); err != nil {
		return false, err
	}
	return false, nil
}

// IsLiked 判断用户是否点赞过
func (s *BlogService) IsLiked(ctx context.Context, blogID, userID int64) (bool, error) {
	key := fmt.Sprintf("%s%d", utils.BLOG_LIKED_KEY, blogID)
	_, err := s.rdb.ZScore(ctx, key, fmt.Sprint(userID)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// TopLikerIDs 返回最早点赞的前 N 个用户ID
func (s *BlogService) TopLikerIDs(ctx context.Context, blogID int64, limit int64) ([]int64, error) {
	key := fmt.Sprintf("%s%d", utils.BLOG_LIKED_KEY, blogID)
	// ZRange：按分数从小到大返回指定区间的成员 
	members, err := s.rdb.ZRange(ctx, key, 0, limit-1).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	var ids []int64
	for _, m := range members {
		if v, err := strconv.ParseInt(m, 10, 64); err == nil {
			ids = append(ids, v)
		}
	}
	return ids, nil
}

// QueryFeed 滚动分页查询关注的笔记流
// lastID 为上次查询的最小时间戳（初次可传 0），offset 处理同分数偏移
func (s *BlogService) QueryFeed(ctx context.Context, userID int64, lastID int64, offset int64, limit int64) ([]model.Blog, int64, int64, error) {
	key := fmt.Sprintf("%s%d", utils.FEED_KEY, userID)
	// +inf 是Redis有序集合按分数查询时的正无穷
	max := "+inf"
	if lastID > 0 {
		max = fmt.Sprintf("%d", lastID)
	}
	// 按分数降序取区间并且返回分数
	zs, err := s.rdb.ZRevRangeByScoreWithScores(ctx, key, &redis.ZRangeBy{
		Min:    "-inf",
		Max:    max,
		Offset: offset,
		Count:  limit,
	}).Result()
	if err != nil {
		return nil, 0, 0, err
	}
	if len(zs) == 0 {
		return nil, 0, 0, nil
	}
	var (
		ids        []int64
		nextLast   int64
		nextOffset int64
	)
	for _, z := range zs {
		if id, err := strconv.ParseInt(fmt.Sprint(z.Member), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	// 计算下一次的 lastID 与 offset（处理同分数情况）
	lastScore := int64(zs[len(zs)-1].Score)
	nextLast = lastScore
	nextOffset = 0
	for i := len(zs) - 1; i >= 0; i-- {
		if int64(zs[i].Score) == lastScore {
			nextOffset++
		}
	}

	// 按查询顺序返回博客列表
	// SELECT ... WHERE id IN (...)  不保证返回顺序，可能乱序
	var blogs []model.Blog
	if err := s.db.WithContext(ctx).
		Where("id IN ?", ids).
		Find(&blogs).Error; err != nil {
		return nil, 0, 0, err
	}
	// 按 ids 顺序排序
	idIndex := make(map[int64]int)
	for i, id := range ids {
		idIndex[id] = i
	}
	// 按idIndex映射的顺序，把blogs排好
	sort.Slice(blogs, func(i, j int) bool {
		return idIndex[blogs[i].ID] < idIndex[blogs[j].ID]
	})

	return blogs, nextLast, nextOffset, nil
}
