import request from '@/utils/request'

export function getTrafficStats(params) {
  return request({
    url: '/api/traffic/stats',
    method: 'get',
    params
  })
} 