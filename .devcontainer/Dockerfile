FROM mcr.microsoft.com/devcontainers/base:bookworm

RUN apt-get update && apt-get upgrade -y
RUN apt-get install -y curl openssl ca-certificates unzip
# sbsigntool (sbverify) and efitools (sig-list-to-certs, sign-efi-sig-list)
# check internal/secureboot's output independently in
# scripts/validate/secureboot-keys-sign-and-verify-a-test-binary.sh (#489);
# efitools' HelloWorld.efi is its test binary.
RUN apt-get install -y sbsigntool efitools
RUN curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc

RUN <<EOT
sh -c 'cat <<EOF > /etc/apt/sources.list.d/zabbly-incus-stable.sources
Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/stable
Suites: $(. /etc/os-release && echo ${VERSION_CODENAME})
Components: main
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/zabbly.asc

EOF'
EOT
RUN apt-get update && apt-get install -y incus-client