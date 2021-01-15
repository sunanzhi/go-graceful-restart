package main

// https://segmentfault.com/a/1190000017986842

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var cfg *srvCfg

type listener struct {
	// Listener address
	Addr string `json:"addr"`
	// Listener file descriptor
	FD int `json:"fd"`
	// Listener file name
	Filename string `json:"filename"`
}

type srvCfg struct {
	sockFile        string
	addr            string
	ln              net.Listener
	shutDownTimeout time.Duration
	childTimeout    time.Duration
}

func main() {
	serve(srvCfg{
		sockFile:        "/tmp/api.sock",
		addr:            ":8000",
		shutDownTimeout: 5 * time.Second,
		childTimeout:    5 * time.Second,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`Hello, world!`))
	}))
}

func serve(config srvCfg, handler http.Handler) {
	cfg = &config
	var err error
	// get tcp listener
	cfg.ln, err = getListener()
	if err != nil {
		panic(err)
	}

	// return an http Server
	srv := start(handler)

	// create a wait routine
	err = waitForSignals(srv)
	if err != nil {
		panic(err)
	}
}

func getListener() (net.Listener, error) {
	// 第一次执行不会 importListener
	ln, err := importListener()
	if err == nil {
		fmt.Printf("imported listener file descriptor for addr: %s\n", cfg.addr)
		return ln, nil
	}
	// 第一次执行会 createListener
	ln, err = createListener()
	if err != nil {
		return nil, err
	}

	return ln, err
}

func importListener() (net.Listener, error) {
	// 向已经准备好的 unix socket 建立连接，这个是爸爸进程在之前就建立好的
	c, err := net.Dial("unix", cfg.sockFile)
	if err != nil {
		fmt.Println("no unix socket now")
		return nil, err
	}
	defer c.Close()
	fmt.Println("准备导入原 listener 文件...")
	var lnEnv string
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func(r io.Reader) {
		defer wg.Done()
		// 读取 conn 中的内容
		buf := make([]byte, 1024)
		n, err := r.Read(buf[:])
		if err != nil {
			return
		}

		lnEnv = string(buf[0:n])
	}(c)
	// 写入 get_listener
	fmt.Println("告诉爸爸我要 'get-listener' 了")
	_, err = c.Write([]byte("get_listener"))
	if err != nil {
		return nil, err
	}

	wg.Wait() // 等待爸爸传给我们参数

	if lnEnv == "" {
		return nil, fmt.Errorf("Listener info not received from socket")
	}

	var l listener
	err = json.Unmarshal([]byte(lnEnv), &l)
	if err != nil {
		return nil, err
	}
	if l.Addr != cfg.addr {
		return nil, fmt.Errorf("unable to find listener for %v", cfg.addr)
	}

	// the file has already been passed to this process, extract the file
	// descriptor and name from the metadata to rebuild/find the *os.File for
	// the listener.
	// 我们已经拿到了监听文件的信息，我们准备自己创建一份新的文件并使用
	lnFile := os.NewFile(uintptr(l.FD), l.Filename)
	fmt.Println("新文件名：", l.Filename)
	if lnFile == nil {
		return nil, fmt.Errorf("unable to create listener file: %v", l.Filename)
	}
	defer lnFile.Close()

	// create a listerer with the *os.File
	ln, err := net.FileListener(lnFile)
	if err != nil {
		return nil, err
	}

	return ln, nil
}

func createListener() (net.Listener, error) {
	fmt.Println("首次创建 listener", cfg.addr)
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return nil, err
	}

	return ln, err
}

func start(handler http.Handler) *http.Server {
	srv := &http.Server{
		Addr:    cfg.addr,
		Handler: handler,
	}
	// start to serve
	go srv.Serve(cfg.ln)
	fmt.Println("server 启动完成，配置信息为：", cfg.ln)
	return srv
}

func waitForSignals(srv *http.Server) error {
	sig := make(chan os.Signal, 1024)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	for {
		select {
		case s := <-sig:
			switch s {
			case syscall.SIGHUP:
				err := handleHangup() // 关闭
				if err == nil {
					// no error occured - child spawned and started
					return shutdown(srv)
				}
			case syscall.SIGTERM, syscall.SIGINT:
				return shutdown(srv)
			}
		}
	}
}

func shutdown(srv *http.Server) error {
	fmt.Println("Server shutting down")
	ctx, cancel := context.WithTimeout(context.Background(),
		cfg.shutDownTimeout)
	defer cancel()

	return srv.Shutdown(ctx)
}

func handleHangup() error {
	c := make(chan string)
	defer close(c)
	errChn := make(chan error)
	defer close(errChn)
	// 开启一个热线通道
	go socketListener(c, errChn)

	for {
		select {
		case cmd := <-c:
			switch cmd {
			case "socket_opened":
				p, err := fork()
				if err != nil {
					fmt.Printf("unable to fork: %v\n", err)
					continue
				}
				fmt.Printf("forked (PID: %d), waiting for spinup", p.Pid)

			case "listener_sent":
				fmt.Println("listener sent - shutting down")

				return nil
			}

		case err := <-errChn:
			return err
		}
	}
}

func socketListener(chn chan<- string, errChn chan<- error) {
	// 创建 socket 服务端
	fmt.Println("创建新的socket通道")
	ln, err := net.Listen("unix", cfg.sockFile)
	if err != nil {
		errChn <- err
		return
	}
	defer ln.Close()

	// signal that we created a socket
	fmt.Println("通道已经打开，可以 fork 了")
	chn <- "socket_opened"

	// accept
	// 阻塞等待子进程连接进来
	c, err := acceptConn(ln)
	if err != nil {
		errChn <- err
		return
	}

	// read from the socket
	buf := make([]byte, 512)
	nr, err := c.Read(buf)
	if err != nil {
		errChn <- err
		return
	}

	data := buf[0:nr]
	fmt.Println("获得消息子进程消息", string(data))
	switch string(data) {
	case "get_listener":
		fmt.Println("子进程请求 listener 信息，开始传送给他吧~")
		err := sendListener(c) // 发送文件描述到新的子进程，用来 import Listener
		if err != nil {
			errChn <- err
			return
		}
		// 传送完毕
		fmt.Println("listener 信息传送完毕")
		chn <- "listener_sent"
	}
}

func acceptConn(l net.Listener) (c net.Conn, err error) {
	chn := make(chan error)
	go func() {
		defer close(chn)
		fmt.Printf("accept 新连接%+v\n", l)
		c, err = l.Accept()
		if err != nil {
			chn <- err
		}
	}()

	select {
	case err = <-chn:
		if err != nil {
			fmt.Printf("error occurred when accepting socket connection: %v\n",
				err)
		}

	case <-time.After(cfg.childTimeout):
		fmt.Println("timeout occurred waiting for connection from child")
	}

	return
}

func sendListener(c net.Conn) error {
	fmt.Printf("发送老的 listener 文件 %+v\n", cfg.ln)
	lnFile, err := getListenerFile(cfg.ln)
	if err != nil {
		return err
	}
	defer lnFile.Close()

	l := listener{
		Addr:     cfg.addr,
		FD:       3, // 文件描述符，进程初始化描述符为0 stdin 1 stdout 2 stderr，所以我们从3开始
		Filename: lnFile.Name(),
	}

	lnEnv, err := json.Marshal(l)
	if err != nil {
		return err
	}
	fmt.Printf("将 %+v\n 写入连接\n", string(lnEnv))
	_, err = c.Write(lnEnv)
	if err != nil {
		return err
	}

	return nil
}

func getListenerFile(ln net.Listener) (*os.File, error) {
	switch t := ln.(type) {
	case *net.TCPListener:
		return t.File()
	case *net.UnixListener:
		return t.File()
	}

	return nil, fmt.Errorf("unsupported listener: %T", ln)
}

func fork() (*os.Process, error) {
	// 拿到原监听文件描述符并打包到元数据中
	lnFile, err := getListenerFile(cfg.ln)
	fmt.Printf("拿到监听文件 %+v\n，开始创建新进程\n", lnFile.Name())
	if err != nil {
		return nil, err
	}
	defer lnFile.Close()

	// 创建子进程时必须要塞的几个文件
	files := []*os.File{
		os.Stdin,
		os.Stdout,
		os.Stderr,
		lnFile,
	}

	// 拿到新进程的程序名，因为我们是重启，所以就是当前运行的程序名字
	execName, err := os.Executable()
	if err != nil {
		return nil, err
	}
	execDir := filepath.Dir(execName)

	// 生孩子了
	p, err := os.StartProcess(execName, []string{execName}, &os.ProcAttr{
		Dir:   execDir,
		Files: files,
		Sys:   &syscall.SysProcAttr{},
	})
	fmt.Println("创建子进程成功")
	if err != nil {
		return nil, err
	}
	// 这里返回 nil 后就会直接 shutdown 爸爸进程
	return p, nil
}
