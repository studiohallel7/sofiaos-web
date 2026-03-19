BIN     := sofiaos
CMD     := ./cmd/server
LDFLAGS := -ldflags="-s -w"

.PHONY: run build deploy tidy

run:
	SOFIAOS_ADDR=:8080 go run $(CMD)/main.go

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BIN) $(CMD)/main.go

# deploy via scp — ajuste o host conforme seu Oracle VPS
deploy: build
	scp $(BIN) oracle:~/sofiaos/sofiaos
	ssh oracle "systemctl restart sofiaos"

tidy:
	go mod tidy

# gera hash bcrypt para um usuário novo:
# make hash pwd=minhasenha
hash:
	@go run -e - <<'EOF'
	package main
	import ("fmt";"golang.org/x/crypto/bcrypt")
	func main(){ h,_:=bcrypt.GenerateFromPassword([]byte("$(pwd)"),12); fmt.Println(string(h)) }
	EOF
