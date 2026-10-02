package memepool

import (
	"errors"
	"fmt"
)

var (
	// ErrNoStorage 没配存储后端。池子还能记元数据，但发不出图。
	ErrNoStorage = errors.New("表情包池未配置存储")
	// ErrEmptyData 空图片字节。
	ErrEmptyData = errors.New("图片内容为空")
	// ErrEmptyDescr 没有描述。描述是模型唯一能用来判断该发哪张的依据，
	// 空描述的图进了池子等于噪声。
	ErrEmptyDescr = errors.New("表情包描述为空")
	// ErrNotFound 池里没有这张图（已被淘汰，或 ID 认错了）。
	ErrNotFound = errors.New("表情包不在池中")
)

// ParseError 落盘文件解析失败。调用方应该隔离掉它并从头开始，
// 而不是让机器人起不来。
type ParseError struct {
	Path string
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("表情包池文件解析失败 %s: %v", e.Path, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// VersionError 落盘文件版本对不上。
type VersionError struct {
	Got, Want int
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("表情包池文件版本 %d，当前程序需要 %d", e.Got, e.Want)
}