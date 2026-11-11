# Copyright (c) The eggshell Authors. All Rights Reserved.
#
# Use of this source code is governed by the license
# that can be found in the LICENSE file.

# eggshell userland built with Nix from the latest nixpkgs (nixos-unstable)
#
#   nix-build nix                    # result/amd64.tar
#   cp result/*.tar rootfs/
#
#   nix-build nix --arg withURoot false
#   nix-build nix --arg sshAuthorizedKeys '[ "ssh-ed25519 AAAA..." ]'
#   nix-build nix --arg withGo true
#   nix-build nix --arg withTests true
{
  nixpkgs ? builtins.fetchTarball "https://github.com/NixOS/nixpkgs/archive/nixos-unstable.tar.gz",
  system ? builtins.currentSystem,
  # add the u-root core commands under /bbin (/bbin/gosh is the shell)
  withURoot ? true,
  # root public keys for the SSH server, no authentication required if empty
  sshAuthorizedKeys ? [ ],
  # add the latest Go toolchain under /usr/local/go (go build, go run, go vet)
  withGo ? false,
  # add the test programs and scripts under /usr/libexec/tests
  withTests ? false,
}:

let
  buildPkgs = import nixpkgs { localSystem = system; };
  lib = buildPkgs.lib;

  # latest stable Go release, as listed by go.dev at evaluation time
  goRelease = builtins.head (
    builtins.fromJSON (builtins.readFile (builtins.fetchurl "https://go.dev/dl/?mode=json"))
  );

  skel = builtins.path {
    name = "eggshell-skel";
    path = ../userland/skel;
  };

  tests = builtins.path {
    name = "eggshell-tests";
    path = ../userland/tests;
  };

  mkRootfs =
    arch:
    {
      config,
      goarch,
      muslArch,
    }:
    let
      # static musl package set for the target
      pkgs =
        (import nixpkgs {
          localSystem = system;
          crossSystem.config = config;
        }).pkgsStatic;

      interp = "ld-musl-${muslArch}.so.1";

      noChecks = {
        doCheck = false;
        doInstallCheck = false;
      };

      bash = pkgs.bashInteractive.overrideAttrs noChecks;

      busybox = pkgs.busybox.override {
        extraConfig = ''
          CONFIG_UNAME_OSNAME "BusyBox/TamaGo"
        '';
      };

      redis =
        (pkgs.redis.override {
          withSystemd = false;
          tlsSupport = false;
        }).overrideAttrs
          (
            old:
            noChecks
            // {
              # redis 8 also builds its test modules by default, with a
              # hardcoded gcc (absent from cross toolchains)
              postPatch = (old.postPatch or "") + ''
                sed -i '/^all:/s/ module_tests//' src/Makefile
                sed -i '/cd xxhash/s/ lib / libxxhash.a /' deps/Makefile
              '';
            }
          );

      authorizedKeys = buildPkgs.writeText "authorized_keys" (
        lib.concatMapStrings (k: k + "\n") sshAuthorizedKeys
      );

      goArchive =
        let
          f = builtins.head (
            builtins.filter (
              f: f.os == "linux" && f.arch == goarch && f.kind == "archive"
            ) goRelease.files
          );
        in
        buildPkgs.fetchurl {
          url = "https://go.dev/dl/${f.filename}";
          inherit (f) sha256;
        };

      goToolchain = buildPkgs.runCommand "go-toolchain-${arch}" { } ''
        tar -C $TMPDIR -xzf ${goArchive}
        g=$TMPDIR/go
        t=pkg/tool/linux_${goarch}

        mkdir -p $out/bin $out/$t
        cp $g/bin/go $g/bin/gofmt $out/bin/
        for c in compile link asm; do cp $g/$t/$c $out/$t/; done
        for c in vet buildid; do [ ! -e $g/$t/$c ] || cp $g/$t/$c $out/$t/; done
        cp -r $g/pkg/include $out/pkg/
        cp $g/VERSION $out/
        sed 's/^GOTOOLCHAIN=.*/GOTOOLCHAIN=local/' $g/go.env > $out/go.env
        echo CGO_ENABLED=0 >> $out/go.env
        tar -C $g --exclude='*_test.go' --exclude=testdata --exclude=src/cmd -cf - src | tar -C $out -xf -
      '';

      # u-root core commands as one Go binary (/bbin/bb)
      uroot = buildPkgs.stdenvNoCC.mkDerivation {
        name = "u-root-${arch}";
        inherit (buildPkgs.u-root) src;
        nativeBuildInputs = [
          buildPkgs.go
          buildPkgs.u-root
          buildPkgs.cpio
        ];
        dontFixup = true;

        buildPhase = ''
          export HOME=$TMPDIR GOCACHE=$TMPDIR/go-cache GOPATH=$TMPDIR/go
          export GOPROXY=off GOFLAGS=-mod=vendor GOTOOLCHAIN=local
          export CGO_ENABLED=0 GOOS=linux GOARCH=${goarch}
          u-root -go-mod=vendor -base=/dev/null -format=cpio -o $TMPDIR/u-root.cpio \
            -defaultsh=gosh -initcmd="" -uinitcmd="" core
        '';

        installPhase = ''
          mkdir -p $out
          (cd $out && cpio -idm --quiet < $TMPDIR/u-root.cpio)
        '';
      };

      # userland/tests, static, plus hello-dynamic using /lib/${interp}
      testPrograms = pkgs.stdenv.mkDerivation {
        name = "eggshell-tests-${arch}";
        src = tests;
        nativeBuildInputs = [ buildPkgs.patchelf ];
        dontPatchELF = true;

        buildPhase = ''
          for f in *.c; do
            $CC -static -O2 -Wall -o "''${f%.c}" "$f" -lpthread
          done
          # pkgsStatic forces static linking through NIX_CFLAGS_LINK
          (unset NIX_CFLAGS_LINK; $CC -O2 -Wall -o hello-dynamic hello.c)
          patchelf --set-interpreter /lib/${interp} --remove-rpath hello-dynamic
        '';

        installPhase = ''
          mkdir -p $out
          for f in *.c; do install -m 755 "''${f%.c}" $out/; done
          install -m 755 hello-dynamic $out/
        '';
      };
    in
    buildPkgs.runCommand "eggshell-rootfs-${arch}" { } ''
      r=$TMPDIR/root
      mkdir -p $r/{bin,sbin,usr/bin,usr/sbin,/usr/libexec,etc,tmp,root,home,var/lib/redis,dev,proc,lib}

      # BusyBox and its applet links (installed in /bin and /sbin)
      install -m 755 ${busybox}/bin/busybox $r/bin/busybox
      for d in bin sbin; do
        [ -d ${busybox}/$d ] || continue
        for f in ${busybox}/$d/*; do
          n=$(basename $f)
          [ "$n" = busybox ] || ln -sf /bin/busybox $r/$d/$n
        done
      done
      ln -s /bin/busybox $r/usr/bin/env

      install -m 755 ${bash}/bin/bash $r/bin/bash

      # musl dynamic loader and C library
      install -m 755 ${pkgs.musl}/lib/libc.so $r/lib/${interp}
      ln -s ${interp} $r/lib/libc.so

      for b in redis-server redis-cli redis-benchmark; do
        install -m 755 ${redis}/bin/$b $r/usr/bin/$b
      done

      # SSH server (sshd package) support: sftp subsystem, authorized keys
      install -m 755 ${pkgs.openssh}/libexec/sftp-server $r/usr/libexec/sftp-server
      ${lib.optionalString (sshAuthorizedKeys != [ ]) ''
        install -d -m 700 $r/root/.ssh
        install -m 600 ${authorizedKeys} $r/root/.ssh/authorized_keys
      ''}

      install -D -m 644 ${buildPkgs.cacert}/etc/ssl/certs/ca-bundle.crt \
        $r/etc/ssl/certs/ca-certificates.crt
      ln -s certs/ca-certificates.crt $r/etc/ssl/cert.pem

      ${lib.optionalString withURoot "cp -a ${uroot}/bbin $r/bbin"}

      ${lib.optionalString withGo ''
        mkdir -p $r/usr/local
        cp -r ${goToolchain} $r/usr/local/go
        ln -s /usr/local/go/bin/go $r/usr/bin/go
        ln -s /usr/local/go/bin/gofmt $r/usr/bin/gofmt
      ''}

      # configuration files and scripts override packaged files
      chmod -R u+w $r
      (cd ${skel} && find . ! -type d) | while read -r f; do rm -f "$r/$f"; done
      cp -rT --no-preserve=ownership ${skel} $r
      chmod -R u+w $r

      ${
        if withTests then
          "mkdir -p $r/usr/libexec/tests && install -m 755 ${testPrograms}/* $r/usr/libexec/tests/"
        else
          "rm -rf $r/usr/libexec/tests"
      }

      mkdir -p $out
      tar -C $r --sort=name --mtime=@1 --owner=0 --group=0 --numeric-owner \
        --format=ustar -cf $out/${arch}.tar .
    '';

  amd64 = mkRootfs "amd64" {
    config = "x86_64-unknown-linux-gnu";
    goarch = "amd64";
    muslArch = "x86_64";
  };
in
buildPkgs.symlinkJoin {
  name = "eggshell-rootfs";
  paths = [
    amd64
  ];
  passthru = { inherit amd64; };
}
