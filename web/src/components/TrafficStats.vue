<template>
  <div class="traffic-stats">
    <div class="stats-header">
      <h2>流量统计</h2>
      <div class="stats-controls">
        <el-radio-group v-model="currentType" @change="handleTypeChange">
          <el-radio-button label="daily">日流量</el-radio-button>
          <el-radio-button label="weekly">周流量</el-radio-button>
          <el-radio-button label="monthly">月流量</el-radio-button>
        </el-radio-group>
        <el-date-picker
          v-model="dateRange"
          type="daterange"
          range-separator="至"
          start-placeholder="开始日期"
          end-placeholder="结束日期"
          @change="handleDateChange"
        />
      </div>
    </div>
    
    <div class="stats-chart">
      <v-chart :option="chartOption" autoresize />
    </div>
    
    <div class="stats-summary">
      <el-row :gutter="20">
        <el-col :span="12">
          <el-card>
            <template #header>总入站流量</template>
            <div class="summary-value">{{ formatBytes(totalInBytes) }}</div>
          </el-card>
        </el-col>
        <el-col :span="12">
          <el-card>
            <template #header>总出站流量</template>
            <div class="summary-value">{{ formatBytes(totalOutBytes) }}</div>
          </el-card>
        </el-col>
      </el-row>
    </div>
  </div>
</template>

<script>
import { use } from 'echarts/core'
import { CanvasRenderer } from 'echarts/renderers'
import { LineChart } from 'echarts/charts'
import { GridComponent, TooltipComponent, LegendComponent } from 'echarts/components'
import VChart from 'vue-echarts'
import { ref, computed, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import dayjs from 'dayjs'

use([CanvasRenderer, LineChart, GridComponent, TooltipComponent, LegendComponent])

export default {
  name: 'TrafficStats',
  components: {
    VChart
  },
  props: {
    serverId: {
      type: Number,
      required: true
    }
  },
  setup(props) {
    const currentType = ref('daily')
    const dateRange = ref([])
    const stats = ref([])
    
    const totalInBytes = computed(() => {
      return stats.value.reduce((sum, stat) => sum + stat.in_bytes, 0)
    })
    
    const totalOutBytes = computed(() => {
      return stats.value.reduce((sum, stat) => sum + stat.out_bytes, 0)
    })
    
    const chartOption = computed(() => ({
      tooltip: {
        trigger: 'axis',
        formatter: (params) => {
          const date = dayjs(params[0].value[0]).format('YYYY-MM-DD HH:mm')
          let result = `${date}<br/>`
          params.forEach(param => {
            result += `${param.seriesName}: ${formatBytes(param.value[1])}<br/>`
          })
          return result
        }
      },
      legend: {
        data: ['入站流量', '出站流量']
      },
      grid: {
        left: '3%',
        right: '4%',
        bottom: '3%',
        containLabel: true
      },
      xAxis: {
        type: 'time',
        boundaryGap: false
      },
      yAxis: {
        type: 'value',
        axisLabel: {
          formatter: (value) => formatBytes(value)
        }
      },
      series: [
        {
          name: '入站流量',
          type: 'line',
          data: stats.value.map(stat => [stat.timestamp * 1000, stat.in_bytes])
        },
        {
          name: '出站流量',
          type: 'line',
          data: stats.value.map(stat => [stat.timestamp * 1000, stat.out_bytes])
        }
      ]
    }))
    
    const fetchStats = async () => {
      try {
        const [start, end] = dateRange.value
        const response = await fetch(`/api/traffic/stats?server_id=${props.serverId}&type=${currentType.value}&start_time=${start.getTime()/1000}&end_time=${end.getTime()/1000}`)
        const data = await response.json()
        stats.value = data.stats
      } catch (error) {
        ElMessage.error('获取流量数据失败')
      }
    }
    
    const handleTypeChange = () => {
      fetchStats()
    }
    
    const handleDateChange = () => {
      fetchStats()
    }
    
    const formatBytes = (bytes) => {
      if (bytes === 0) return '0 B'
      const k = 1024
      const sizes = ['B', 'KB', 'MB', 'GB', 'TB']
      const i = Math.floor(Math.log(bytes) / Math.log(k))
      return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i]
    }
    
    onMounted(() => {
      // 设置默认日期范围为最近7天
      const end = new Date()
      const start = new Date()
      start.setDate(start.getDate() - 7)
      dateRange.value = [start, end]
      fetchStats()
    })
    
    return {
      currentType,
      dateRange,
      chartOption,
      totalInBytes,
      totalOutBytes,
      handleTypeChange,
      handleDateChange,
      formatBytes
    }
  }
}
</script>

<style scoped>
.traffic-stats {
  padding: 20px;
}

.stats-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 20px;
}

.stats-controls {
  display: flex;
  gap: 20px;
}

.stats-chart {
  height: 400px;
  margin-bottom: 20px;
}

.stats-summary {
  margin-top: 20px;
}

.summary-value {
  font-size: 24px;
  font-weight: bold;
  color: #409EFF;
}
</style> 