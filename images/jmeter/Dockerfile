FROM eclipse-temurin:21-jre-ubi9-minimal

ADD https://dlcdn.apache.org/jmeter/binaries/apache-jmeter-5.6.3.tgz /tmp/

WORKDIR /jmeter-workspace

RUN tar -xzf /tmp/apache-jmeter-5.6.3.tgz -C /tmp/ && \
    rm /tmp/apache-jmeter-5.6.3.tgz && \
    mv /tmp/apache-jmeter-5.6.3 /opt/jmeter && chown -R 1000:1000 /jmeter-workspace

ENV PATH="/opt/jmeter/bin:${PATH}"

ENV HOME=/jmeter-workspace

USER 1000

ENTRYPOINT ["/opt/jmeter/bin/jmeter.sh"]