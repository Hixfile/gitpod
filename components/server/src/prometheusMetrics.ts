import * as prometheusClient from 'prom-client';

// Enable collection of default metrics.
prometheusClient.collectDefaultMetrics({ timeout: 5000 });
export const register = prometheusClient.register

const loginCounter = new prometheusClient.Counter({
    name: 'gitpod_login_requests_total',
	help: 'Total amount of login requests',
    labelNames: ['status', 'oauth'],
    registers: [register]
});

export function increaseLoginCounter(status: string, oauth: string){
    loginCounter.inc({
        status: status,
        oauth: oauth,
    })
}