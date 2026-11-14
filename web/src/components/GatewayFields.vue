<script setup lang="ts">
import {computed} from 'vue';
import {NFormItem, NInput, NInputNumber, NSelect} from 'naive-ui';
import {useI18n} from 'vue-i18n';
import type {ClusterOptions, Component} from '../api';
import {ingressEndpointURL, loadBalancerSSHAddress} from '../utils/gateway';
import MeasurementInput from './MeasurementInput.vue';

const props = defineProps<{
  gateway: NonNullable<Component['gateway']>;
  cluster: ClusterOptions;
}>();
const trustedProxies = defineModel<string>('trustedProxies', {required: true});
const {t} = useI18n();

const ingressClassOptions = computed(() =>
  props.cluster.ingressClasses.map(({name, controller, default: isDefault}) => ({
    label: `${name} (${isDefault ? t('component.defaultIngressClass') : controller})`,
    value: name,
  })),
);
const tlsSecretOptions = computed(() => props.cluster.tlsSecrets.map((name) => ({label: name, value: name})));
const issuerOptions = computed(() =>
  props.cluster.issuers.map(({kind, name}) => ({label: `${kind}/${name}`, value: `${kind}/${name}`})),
);
const endpointPlaceholder = computed(() =>
  props.gateway.httpExposure.ingress?.tls.source === 'none'
    ? 'http://codespace.example.com'
    : 'https://codespace.example.com',
);

function setHTTPExposure(value: 'external' | 'ingress') {
  const source = /^https:\/\//i.test(props.gateway.endpointBaseURL) ? 'secret' : 'none';
  props.gateway.httpExposure =
    value === 'external'
      ? {mode: 'external'}
      : {
          mode: 'ingress',
          ingress: {
            className: props.cluster.ingressClasses.find(({default: isDefault}) => isDefault)?.name ?? '',
            tls: {source},
          },
        };
  if (value === 'ingress') props.gateway.endpointBaseURL = ingressEndpointURL(props.gateway.endpointBaseURL, source);
}

function setTLS(source: 'none' | 'secret' | 'certManager') {
  const ingress = props.gateway.httpExposure.ingress;
  if (!ingress) return;
  ingress.tls = {source};
  props.gateway.endpointBaseURL = ingressEndpointURL(props.gateway.endpointBaseURL, source);
}

function setIssuer(value: string) {
  const tls = props.gateway.httpExposure.ingress?.tls;
  if (!tls) return;
  const [kind, ...name] = value.split('/');
  tls.issuer = {kind: kind as 'Issuer' | 'ClusterIssuer', name: name.join('/')};
}

function setSSHExposure(value: 'external' | 'loadBalancer') {
  props.gateway.sshExposure = {mode: value};
  if (value === 'loadBalancer')
    props.gateway.sshExternalAddress = loadBalancerSSHAddress(props.gateway.sshExternalAddress);
}
</script>

<template>
  <h2>{{ t('component.gatewayAddresses') }}</h2>
  <div class="form-grid">
    <NFormItem :label="t('component.publicHTTPS')">
      <NInput
        v-model:value="gateway.endpointBaseURL"
        :placeholder="endpointPlaceholder"
        required
      />
    </NFormItem>
    <NFormItem :label="t('component.publicSSH')">
      <NInput
        v-model:value="gateway.sshExternalAddress"
        placeholder="codespace.example.com:22"
        required
      />
    </NFormItem>
  </div>
  <h2>{{ t('component.publicExposure') }}</h2>
  <p class="section-help">{{ t('component.publicExposureHelp') }}</p>
  <div class="form-grid">
    <NFormItem :label="t('component.httpExposure')">
      <NSelect
        :value="gateway.httpExposure.mode"
        :options="[
          {label: t('component.externalExposure'), value: 'external'},
          {label: t('component.ingressExposure'), value: 'ingress'},
        ]"
        @update:value="setHTTPExposure"
      />
    </NFormItem>
    <NFormItem :label="t('component.sshExposure')">
      <NSelect
        :value="gateway.sshExposure.mode"
        :options="[
          {label: t('component.externalExposure'), value: 'external'},
          {label: t('component.loadBalancerExposure'), value: 'loadBalancer'},
        ]"
        @update:value="setSSHExposure"
      />
    </NFormItem>
  </div>
  <div
    v-if="gateway.httpExposure.ingress"
    class="form-grid"
  >
    <NFormItem :label="t('component.ingressClass')">
      <NSelect
        v-model:value="gateway.httpExposure.ingress.className"
        :options="ingressClassOptions"
      />
    </NFormItem>
    <NFormItem :label="t('component.tlsSource')">
      <NSelect
        :value="gateway.httpExposure.ingress.tls.source"
        :options="[
          {label: t('component.noTLS'), value: 'none'},
          {label: t('component.existingTLSSecret'), value: 'secret'},
          {label: t('component.certManagerIssuer'), value: 'certManager'},
        ]"
        @update:value="setTLS"
      />
    </NFormItem>
    <NFormItem
      v-if="gateway.httpExposure.ingress.tls.source === 'secret'"
      :label="t('component.tlsSecret')"
    >
      <NSelect
        v-model:value="gateway.httpExposure.ingress.tls.secretName"
        :options="tlsSecretOptions"
      />
    </NFormItem>
    <NFormItem
      v-if="gateway.httpExposure.ingress.tls.source === 'certManager'"
      :label="t('component.issuer')"
    >
      <NSelect
        :value="
          gateway.httpExposure.ingress.tls.issuer
            ? `${gateway.httpExposure.ingress.tls.issuer.kind}/${gateway.httpExposure.ingress.tls.issuer.name}`
            : ''
        "
        :options="issuerOptions"
        @update:value="setIssuer"
      />
    </NFormItem>
  </div>
  <div class="form-grid">
    <NFormItem
      class="field-wide"
      :label="t('component.trustedProxies')"
      :feedback="t('component.trustedProxiesHelp')"
    >
      <NInput
        v-model:value="trustedProxies"
        type="textarea"
        :autosize="{minRows: 2, maxRows: 5}"
        placeholder="10.0.0.0/8"
      />
    </NFormItem>
  </div>
  <h2>{{ t('component.sessions') }}</h2>
  <div class="form-grid">
    <NFormItem
      :label="t('component.sessionLifetime')"
      :feedback="t('component.sessionLifetimeHelp')"
    >
      <MeasurementInput
        v-model="gateway.sessions.ttl"
        kind="duration"
        :aria-label="t('component.sessionLifetime')"
      />
    </NFormItem>
    <NFormItem :label="t('component.idleTimeout')">
      <MeasurementInput
        v-model="gateway.sessions.idleTimeout"
        kind="duration"
        :aria-label="t('component.idleTimeout')"
      />
    </NFormItem>
    <NFormItem :label="t('component.authRefresh')">
      <MeasurementInput
        v-model="gateway.sessions.revalidateInterval"
        kind="duration"
        :aria-label="t('component.authRefresh')"
      />
    </NFormItem>
    <NFormItem :label="t('component.sessionsPerCodespace')">
      <NInputNumber
        v-model:value="gateway.sessions.maxPerCodespace"
        :min="1"
      />
    </NFormItem>
    <NFormItem :label="t('component.sessionsPerUser')">
      <NInputNumber
        v-model:value="gateway.sessions.maxPerUser"
        :min="1"
      />
    </NFormItem>
    <NFormItem :label="t('component.inflightTotal')">
      <NInputNumber
        v-model:value="gateway.limits.maxInflightTotal"
        :min="1"
      />
    </NFormItem>
    <NFormItem :label="t('component.inflightSession')">
      <NInputNumber
        v-model:value="gateway.limits.maxInflightPerSession"
        :min="1"
      />
    </NFormItem>
    <NFormItem :label="t('component.sshChannels')">
      <NInputNumber
        v-model:value="gateway.ssh.maxChannelsPerConnection"
        :min="1"
      />
    </NFormItem>
  </div>
</template>
