FROM livekit/gstreamer:1.20.4-dev

ARG TARGETPLATFORM
ARG CEF_VERSION

RUN apt-get update && \
    apt-get install -y \
      build-essential \
      ca-certificates \
      cmake \
      git \
      libasound2 \
      libatk-bridge2.0-0 \
      libatk1.0-0 \
      libc6 \
      libcairo2 \
      libcups2 \
      libdbus-1-3 \
      libexpat1 \
      libfontconfig1 \
      libgbm1 \
      libgcc1 \
      libglib2.0-0 \
      libnspr4 \
      libnss3 \
      libpango-1.0-0 \
      libpangocairo-1.0-0 \
      libstdc++6 \
      libx11-6 \
      libx11-xcb1 \
      libxcb1 \
      libxcomposite1 \
      libxcursor1 \
      libxdamage1 \
      libxext6 \
      libxfixes3 \
      libxi6 \
      libxrandr2 \
      libxrender1 \
      libxss1 \
      libxtst6 \
      pkg-config

RUN git clone https://github.com/livekit/gstcefsrc.git
RUN mkdir -p gstcefsrc/third_party/cef && mkdir gstcefsrc/build

#COPY ./cef_binary_${CEF_VERSION}_linuxarm64.tar.bz2 gstcefsrc/third_party/cef

#WORKDIR /gstcefsrc/third_party/cef
#RUN if [ "$TARGETPLATFORM" = "linux/arm64" ] \
#    ; then tar xjf cef_binary_${CEF_VERSION}_linuxarm64.tar.bz2 \
#    ; else tar xjf cef_binary_${CEF_VERSION}_linux64.tar.bz2 \
#    ; fi

WORKDIR /gstcefsrc/build
RUN cmake -G "Unix Makefiles" -DCMAKE_BUILD_TYPE=Release ..
#RUN cmake -G "Unix Makefiles" -DCEF_VERSION=${CEF_VERSION} -DCMAKE_BUILD_TYPE=Release ..
RUN make

#docker build --build-arg CEF_VERSION="109.0.1+gcd5e37a+chromium-109.0.5414.8" -t gstcefsrc ./build/gstcefsrc
#docker run --rm -it --entrypoint bash gstcefsrc

#RUN apt-get install -y \
#    curl \
#    ffmpeg \
#    fonts-noto \
#    gnupg \
#    golang \
#    gstreamer1.0-pulseaudio \
#    pulseaudio \
#    unzip \
#    wget \
#    xvfb

#Xvfb :3 -screen 0 1920x1080x24 -ac -nolisten tcp &
#GST_PLUGIN_PATH=/gstcefsrc/build/Release DISPLAY=":3" gst-launch-1.0 cefsrc url="https://soundcloud.com/platform/sama" ! video/x-raw, width=1920, height=1080, framerate=60/1 ! cefdemux name=d d.video ! queue max-size-bytes=0 max-size-buffers=0 max-size-time=3000000000 ! videoconvert ! fakevideosink  audiotestsrc do-timestamp=true is-live=true  volume=0.00 ! audiomixer name=mix ! queue max-size-bytes=0 max-size-buffers=0 max-size-time=3000000000 ! audioconvert ! fakeaudiosink  d.audio ! mix.
