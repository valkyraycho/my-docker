FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
    iproute2 \       
    iptables \                              
    iputils-ping \                                        
    curl \                               
    ca-certificates \                             
    procps \                                              
    && rm -rf /var/lib/apt/lists/*