```shell
export CHROME_BUILDER={{ ip }}
ssh root@$CHROME_BUILDER
```

```shell
adduser chrome
adduser chrome sudo
su - chrome

mkdir code
mkdir code/automate
mkdir code/chromium_git

sudo apt-get update
sudo apt-get install -y \
  apt-utils \
  build-essential \
  curl \
  git \
  lsb-release \
  python3 \
  sudo \
  zip

cd code
curl 'https://chromium.googlesource.com/chromium/src/+/refs/heads/main/build/install-build-deps.sh?format=TEXT' | base64 -d > install-build-deps.sh
chmod 755 install-build-deps.sh
sudo ./install-build-deps.sh --no-chromeos-fonts --no-nacl

git clone https://chromium.googlesource.com/chromium/tools/depot_tools.git
export PATH="$PATH:/home/chrome/code/depot_tools"

cd automate
wget https://bitbucket.org/chromiumembedded/cef/raw/master/tools/automate/automate-git.py
export CEF_USE_GN=1
export CEF_INSTALL_SYSROOT=arm64
export GN_DEFINES="proprietary_codecs=true ffmpeg_branding=Chrome is_official_build=true use_sysroot=true symbol_level=1 is_cfi=false use_thin_lto=false chrome_pgo_phase=0"
export CEF_ARCHIVE_FORMAT=tar.bz2
python3 automate-git.py \
  --download-dir=/home/chrome/code/chromium_git \
  --depot-tools-dir=/home/chrome/code/depot_tools \
  --branch=5414 \
  --build-target=cefsimple \
  --no-debug-build \
  --arm64-build
```

```shell
scp root@$CHROME_BUILDER:/home/chrome/code/chromium_git/chromium/src/cef/binary_distrib/cef_binary_\*_linuxarm64.tar.bz2 ~/livekit/egress/build/cef/
```

```shell
scp root@$CHROME_BUILDER:/home/chrome/code/chromium_git/chromium/src/cef/binary_distrib/cef_binary_\*_linuxamd64.tar.bz2 ~/livekit/egress/build/cef/
```

# cd ~/code/chromium_git/chromium/src/cef
# ./cef_create_projects.sh
# cd ~/code/chromium_git/chromium/src
# ninja -C out/Release_GN_arm64 cefsimple chrome_sandbox
