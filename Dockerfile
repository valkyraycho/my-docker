FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
    jq \
    iproute2 \       
    iptables \                              
    iputils-ping \                                        
    curl \                               
    ca-certificates \                             
    procps \                                              
    && rm -rf /var/lib/apt/lists/*