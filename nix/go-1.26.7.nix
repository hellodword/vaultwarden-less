{ pkgs }:

pkgs.stdenv.mkDerivation {
  pname = "go";
  version = "1.26.7";

  src = pkgs.fetchurl {
    url = "https://go.dev/dl/go1.26.7.linux-amd64.tar.gz";
    hash = "sha256-/7X43hDGJVDf3atms2tXAwch4KRKMhjp4Rgde1nxIco=";
  };

  nativeBuildInputs = [ pkgs.makeWrapper ];
  dontConfigure = true;
  dontBuild = true;

  installPhase = ''
    runHook preInstall
    mkdir -p "$out/lib/go"
    cp -R . "$out/lib/go"
    makeWrapper "$out/lib/go/bin/go" "$out/bin/go" --set GOROOT "$out/lib/go"
    makeWrapper "$out/lib/go/bin/gofmt" "$out/bin/gofmt" --set GOROOT "$out/lib/go"
    runHook postInstall
  '';
}
