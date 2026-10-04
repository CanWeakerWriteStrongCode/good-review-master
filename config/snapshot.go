package config

// Snapshot 返回当前配置快照。
//
// 它是个**函数**而不是 *Config，这个形状本身就是设计的一部分：
// 字段是 *Config 的组件会不自觉地把它当常量缓存下来（"反正启动时读一次"），
// 热更新对它就静默失效——不报错，只是新配置不生效。
// 每次写 cfg() 都在提醒"这份配置是会被整体换掉的"。
//
// 约定：**用到的时候调一次，不要把返回值存进字段或局部变量长期持有。**
//
// 它由 config/store 的 Store.Get 直接满足（方法值的类型恰好是 func() *Config），
// 所以 config 与 config/store 两个包之间不需要互相 import。
type Snapshot func() *Config
