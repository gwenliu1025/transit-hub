package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(command(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Getenv))
}

func command(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, getenv func(string) string) int {
	flags := flag.NewFlagSet("reconcile-dashboard", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("input", "", "已验证营收JSON文件，-表示标准输入")
	apply := flags.Bool("apply", false, "事务内执行；默认只读预演")
	expected := flags.String("expected-plan-sha256", "", "已审阅的dry-run计划哈希")
	emit := func(r report, message string) int {
		if r.Status == "" {
			r.Status = "not_started"
		}
		r.Error = message
		if json.NewEncoder(stdout).Encode(r) != nil {
			return 1
		}
		if message != "" {
			return 1
		}
		return 0
	}
	if flags.Parse(args) != nil || flags.NArg() != 0 || *path == "" {
		return emit(report{}, "参数无效；使用--input文件或-，执行需--apply及--expected-plan-sha256")
	}
	if (*apply && *expected == "") || (!*apply && *expected != "") {
		return emit(report{}, "apply必须且只能同时指定expected-plan-sha256")
	}
	reader := stdin
	if *path != "-" {
		file, err := os.Open(*path)
		if err != nil {
			return emit(report{}, "输入文件打开失败")
		}
		defer file.Close()
		reader = file
	}
	input, err := decodeInput(reader, time.Now())
	if err != nil {
		return emit(report{}, err.Error())
	}
	url := strings.TrimSpace(getenv("DATABASE_URL"))
	if url == "" {
		return emit(report{}, "未配置DATABASE_URL")
	}
	// 驱动连接错误可能携带地址和凭据，只输出阶段，不透传连接串或原始错误。
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return emit(report{}, "DATABASE_URL配置无效")
	}
	config.MaxConns = 2
	config.ConnConfig.ConnectTimeout = 10 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return emit(report{}, "数据库连接初始化失败")
	}
	defer pool.Close()
	result, err := runRepair(ctx, postgresDatabase{pool: pool}, input, *apply, *expected)
	if err != nil {
		return emit(result, err.Error())
	}
	return emit(result, "")
}
