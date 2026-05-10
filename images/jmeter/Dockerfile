FROM eclipse-temurin:21-jre-ubi9-minimal

WORKDIR /jmeter-workspace

RUN curl https://dlcdn.apache.org/jmeter/binaries/apache-jmeter-5.6.3.tgz -o /tmp/apache-jmeter-5.6.3.tgz && \
    tar -xzf /tmp/apache-jmeter-5.6.3.tgz -C /opt/ && \
    rm /tmp/apache-jmeter-5.6.3.tgz && ln -s /opt/apache-jmeter-5.6.3 /opt/jmeter && \
    curl -L -o /tmp/jolokia-agent-jvm-2.6.0-javaagent.jar https://repo1.maven.org/maven2/org/jolokia/jolokia-agent-jvm/2.6.0/jolokia-agent-jvm-2.6.0-javaagent.jar && \
    mv /tmp/jolokia-agent-jvm-2.6.0-javaagent.jar /opt/jmeter/ && \
    curl -L -o /tmp/plugin-manager.jar https://jmeter-plugins.org/get/ && \
    mv /tmp/plugin-manager.jar /opt/jmeter/lib/ext/ && \
    adduser jmeter -d /jmeter-workspace && \
    chown -R jmeter:jmeter /jmeter-workspace

ENV PATH="/opt/jmeter/bin:${PATH}"

ENV JVM_ARGS=" -javaagent:/opt/jmeter/jolokia-agent-jvm-2.6.0-javaagent.jar "

ENV HOME=/jmeter-workspace

USER jmeter

ENTRYPOINT ["/opt/jmeter/bin/jmeter.sh"]