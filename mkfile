INSTALL_PATH=$HOME/bin

all:V: install

build:V:
	go build -o $INSTALL_PATH/olliesrv .

install:V: build
	mkdir -p $HOME/.local/share/ollie/tools
	cp -f tools/* $HOME/.local/share/ollie/tools/

clean:V:
	rm -f $INSTALL_PATH/olliesrv
