{
  description = "SRE agent — Go toolchain and the CLIs the scripts shell out to";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      # No flake-utils: one dependency for a genAttrs call is not worth the
      # extra input to lock and update.
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" "x86_64-darwin" ];
      forAllSystems = f:
        nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            # go.mod asks for 1.26.5 and nixpkgs has exactly that, which is why
            # GOTOOLCHAIN below can be pinned to local.
            go
            gopls
            gotools # goimports
            delve

            # What scripts/ actually shells out to. Checked against the scripts
            # rather than guessed: enqueue.sh and deploy.sh need aws, demo.sh
            # and deploy.sh parse JSON with python3, most of them use jq and
            # curl. `cockroach` appears only in a comment, so it is not here.
            awscli2
            curl
            jq
            python3

            # psql, for poking CockroachDB Cloud directly. That connection
            # string needs &sslrootcert=system appended.
            postgresql

            # The CLI only. The daemon is a system-level service and a devShell
            # cannot provide one — see the note in shellHook.
            docker-client
          ];

          # The race detector goes through cgo, and NixOS's default
          # FORTIFY_SOURCE makes that noisy. scripts/test.sh -r is the gate
          # before committing, so it needs to be quiet enough to read.
          hardeningDisable = [ "fortify" ];

          env = {
            # Never silently download a second toolchain. nixpkgs' Go already
            # satisfies go.mod, so a future mismatch should be an error that
            # names itself rather than a background fetch.
            GOTOOLCHAIN = "local";
          };

          shellHook = ''
            echo "SRE agent: $(go version | cut -d' ' -f3), $(docker --version 2>/dev/null || echo 'docker CLI')"

            # The one thing that actually blocks remediation work, said once at
            # the point where it can be acted on rather than 20 minutes into a
            # sandbox run.
            if ! docker version >/dev/null 2>&1; then
              echo "  no docker daemon reachable: cmd/sandboxcheck and remediation will not run."
              echo "  On NixOS: virtualisation.docker.enable = true; (or use podman and set CONTAINER_BINARY)."
            fi

            # Deliberately not sourced. utils.LoadConfig reads ../.env itself,
            # and exporting secrets into every subprocess of an interactive
            # shell is a good way to get them into a log.
            echo "  credentials come from ../.env, loaded by the app, not by this shell."
          '';
        };
      });
    };
}
