{
  description = "Hecate — the promotion and release-orchestration layer for FluxCD";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    agenix.url = "github:ryantm/agenix";
    agenix.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    { self
    , nixpkgs
    , agenix
    , ...
    }:
    let
      # Bump in lockstep with the git tag when cutting a release.
      version = "0.1.0";

      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      hecatePackage =
        pkgs:
        pkgs.buildGoModule {
          pname = "hecate";
          inherit version;

          # `self` copies only git-tracked files into the store, so build
          # artefacts and untracked scratch never reach the derivation.
          src = self;

          vendorHash = "sha256-RALvhuNvRr+nrRZW0ekei9dh/D/L96JmZqkTbPH+N54=";

          subPackages = [
            "cmd/hecate-controller"
            "cmd/hecate"
            "cmd/hecate-mcp"
          ];

          # Pure Go: hermetic builds that cross-compile cleanly.
          env.CGO_ENABLED = 0;
          ldflags = [
            "-s"
            "-w"
            "-X main.version=${version}"
          ];

          meta = {
            description = "Promotion and release orchestration for FluxCD";
            homepage = "https://github.com/olafkfreund/Hecate";
            license = pkgs.lib.licenses.asl20;
            mainProgram = "hecate-controller";
            platforms = pkgs.lib.platforms.unix;
          };
        };
    in
    {
      packages = forAllSystems (pkgs: {
        default = hecatePackage pkgs;
        hecate-controller = hecatePackage pkgs;
      });

      # `nix flake check` builds the package and runs the full suite. Both are
      # cluster-free by design, so this is the same gate CI enforces.
      checks = forAllSystems (pkgs: {
        package = hecatePackage pkgs;

        tests = pkgs.runCommand "hecate-tests"
          {
            nativeBuildInputs = [
              pkgs.go
              pkgs.git
            ];
          }
          ''
            cp -r ${self} src && chmod -R u+w src && cd src
            export HOME=$TMPDIR GOFLAGS=-mod=mod GOPATH=$TMPDIR/go GOCACHE=$TMPDIR/cache
            go vet ./... && go test ./...
            touch $out
          '';
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          name = "hecate";

          packages = [
            # Go toolchain
            pkgs.go
            pkgs.gopls
            pkgs.golangci-lint
            pkgs.delve

            # Kubernetes: controller-gen generates the CRDs and RBAC in charts/
            pkgs.kubernetes-controller-tools
            pkgs.kubectl
            pkgs.kubernetes-helm
            pkgs.k3d
            pkgs.fluxcd

            # Secrets
            agenix.packages.${pkgs.stdenv.hostPlatform.system}.default
            pkgs.age

            # Nix hygiene
            pkgs.nixfmt
            pkgs.statix
            pkgs.deadnix
            pkgs.nixd

            pkgs.jq
            pkgs.yq-go
            pkgs.gnumake
          ];

          shellHook = ''
            # KUBECONFIG is scoped to the repo so a dev cluster can never be
            # confused with a real one. `make cluster` writes it.
            export KUBECONFIG="$PWD/.dev/kubeconfig"
            export HECATE_DEV_CLUSTER="hecate-dev"
            mkdir -p .dev

            cat <<'EOF'
            Hecate dev shell

              make test        cluster-free suite (the bar: everything that can be)
              make generate    regenerate CRDs and RBAC into charts/
              make cluster     k3d cluster in Docker, with Flux installed
              make cluster-rm  tear it down
              make e2e         install Hecate into the dev cluster and exercise it

            KUBECONFIG is pinned to ./.dev/kubeconfig — never your real cluster.
            EOF
          '';
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
