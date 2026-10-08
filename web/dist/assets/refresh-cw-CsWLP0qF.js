import{Cn as e,En as t,Gn as n,H as r,L as i,Nn as a,P as o,Pn as s,Qt as c,S as l,Un as u,Vn as d,Zt as f,_n as p,a as m,bn as h,bt as g,en as _,et as v,gn as y,gt as b,it as x,mt as S,n as C,nn as w,nt as T,ot as E,rn as D,tn as O,ut as k,vn as A,vt as j,x as M,yt as N}from"./index-BcYaLBkz.js";var P={closeIconSizeTiny:`12px`,closeIconSizeSmall:`12px`,closeIconSizeMedium:`14px`,closeIconSizeLarge:`14px`,closeSizeTiny:`16px`,closeSizeSmall:`16px`,closeSizeMedium:`18px`,closeSizeLarge:`18px`,padding:`0 7px`,closeMargin:`0 0 0 4px`};function F(e){let{textColor2:t,primaryColorHover:n,primaryColorPressed:r,primaryColor:i,infoColor:a,successColor:o,warningColor:s,errorColor:c,baseColor:l,borderColor:u,opacityDisabled:d,tagColor:f,closeIconColor:p,closeIconColorHover:m,closeIconColorPressed:h,borderRadiusSmall:g,fontSizeMini:_,fontSizeTiny:v,fontSizeSmall:y,fontSizeMedium:b,heightMini:x,heightTiny:S,heightSmall:C,heightMedium:w,closeColorHover:T,closeColorPressed:D,buttonColor2Hover:O,buttonColor2Pressed:k,fontWeightStrong:A}=e;return{...P,closeBorderRadius:g,heightTiny:x,heightSmall:S,heightMedium:C,heightLarge:w,borderRadius:g,opacityDisabled:d,fontSizeTiny:_,fontSizeSmall:v,fontSizeMedium:y,fontSizeLarge:b,fontWeightStrong:A,textColorCheckable:t,textColorHoverCheckable:t,textColorPressedCheckable:t,textColorChecked:l,colorCheckable:`#0000`,colorHoverCheckable:O,colorPressedCheckable:k,colorChecked:i,colorCheckedHover:n,colorCheckedPressed:r,border:`1px solid ${u}`,textColor:t,color:f,colorBordered:`rgb(250, 250, 252)`,closeIconColor:p,closeIconColorHover:m,closeIconColorPressed:h,closeColorHover:T,closeColorPressed:D,borderPrimary:`1px solid ${E(i,{alpha:.3})}`,textColorPrimary:i,colorPrimary:E(i,{alpha:.12}),colorBorderedPrimary:E(i,{alpha:.1}),closeIconColorPrimary:i,closeIconColorHoverPrimary:i,closeIconColorPressedPrimary:i,closeColorHoverPrimary:E(i,{alpha:.12}),closeColorPressedPrimary:E(i,{alpha:.18}),borderInfo:`1px solid ${E(a,{alpha:.3})}`,textColorInfo:a,colorInfo:E(a,{alpha:.12}),colorBorderedInfo:E(a,{alpha:.1}),closeIconColorInfo:a,closeIconColorHoverInfo:a,closeIconColorPressedInfo:a,closeColorHoverInfo:E(a,{alpha:.12}),closeColorPressedInfo:E(a,{alpha:.18}),borderSuccess:`1px solid ${E(o,{alpha:.3})}`,textColorSuccess:o,colorSuccess:E(o,{alpha:.12}),colorBorderedSuccess:E(o,{alpha:.1}),closeIconColorSuccess:o,closeIconColorHoverSuccess:o,closeIconColorPressedSuccess:o,closeColorHoverSuccess:E(o,{alpha:.12}),closeColorPressedSuccess:E(o,{alpha:.18}),borderWarning:`1px solid ${E(s,{alpha:.35})}`,textColorWarning:s,colorWarning:E(s,{alpha:.15}),colorBorderedWarning:E(s,{alpha:.12}),closeIconColorWarning:s,closeIconColorHoverWarning:s,closeIconColorPressedWarning:s,closeColorHoverWarning:E(s,{alpha:.12}),closeColorPressedWarning:E(s,{alpha:.18}),borderError:`1px solid ${E(c,{alpha:.23})}`,textColorError:c,colorError:E(c,{alpha:.1}),colorBorderedError:E(c,{alpha:.08}),closeIconColorError:c,closeIconColorHoverError:c,closeIconColorPressedError:c,closeColorHoverError:E(c,{alpha:.12}),closeColorPressedError:E(c,{alpha:.18})}}var I={name:`Tag`,common:x,self:F},L={color:Object,type:{type:String,default:`default`},round:Boolean,size:String,closable:Boolean,disabled:{type:Boolean,default:void 0}},R=c(`tag`,`
 --n-close-margin: var(--n-close-margin-top) var(--n-close-margin-right) var(--n-close-margin-bottom) var(--n-close-margin-left);
 white-space: nowrap;
 position: relative;
 box-sizing: border-box;
 cursor: default;
 display: inline-flex;
 align-items: center;
 flex-wrap: nowrap;
 padding: var(--n-padding);
 border-radius: var(--n-border-radius);
 color: var(--n-text-color);
 background-color: var(--n-color);
 transition: 
 border-color .3s var(--n-bezier),
 background-color .3s var(--n-bezier),
 color .3s var(--n-bezier),
 box-shadow .3s var(--n-bezier),
 opacity .3s var(--n-bezier);
 line-height: 1;
 height: var(--n-height);
 font-size: var(--n-font-size);
`,[O(`strong`,`
 font-weight: var(--n-font-weight-strong);
 `),_(`border`,`
 pointer-events: none;
 position: absolute;
 left: 0;
 right: 0;
 top: 0;
 bottom: 0;
 border-radius: inherit;
 border: var(--n-border);
 transition: border-color .3s var(--n-bezier);
 `),_(`icon`,`
 display: flex;
 margin: 0 4px 0 0;
 color: var(--n-text-color);
 transition: color .3s var(--n-bezier);
 font-size: var(--n-avatar-size-override);
 `),_(`avatar`,`
 display: flex;
 margin: 0 6px 0 0;
 `),_(`close`,`
 margin: var(--n-close-margin);
 transition:
 background-color .3s var(--n-bezier),
 color .3s var(--n-bezier);
 `),O(`round`,`
 padding: 0 calc(var(--n-height) / 3);
 border-radius: calc(var(--n-height) / 2);
 `,[_(`icon`,`
 margin: 0 4px 0 calc((var(--n-height) - 8px) / -2);
 `),_(`avatar`,`
 margin: 0 6px 0 calc((var(--n-height) - 8px) / -2);
 `),O(`closable`,`
 padding: 0 calc(var(--n-height) / 4) 0 calc(var(--n-height) / 3);
 `)]),O(`icon, avatar`,[O(`round`,`
 padding: 0 calc(var(--n-height) / 3) 0 calc(var(--n-height) / 2);
 `)]),O(`disabled`,`
 cursor: not-allowed !important;
 opacity: var(--n-opacity-disabled);
 `),O(`checkable`,`
 cursor: pointer;
 box-shadow: none;
 color: var(--n-text-color-checkable);
 background-color: var(--n-color-checkable);
 `,[w(`disabled`,[f(`&:hover`,`background-color: var(--n-color-hover-checkable);`,[w(`checked`,`color: var(--n-text-color-hover-checkable);`)]),f(`&:active`,`background-color: var(--n-color-pressed-checkable);`,[w(`checked`,`color: var(--n-text-color-pressed-checkable);`)])]),O(`checked`,`
 color: var(--n-text-color-checked);
 background-color: var(--n-color-checked);
 `,[w(`disabled`,[f(`&:hover`,`background-color: var(--n-color-checked-hover);`),f(`&:active`,`background-color: var(--n-color-checked-pressed);`)])])])]),z=[`onClick`,`onMouseenter`,`onMouseleave`],B={...v.props,...L,bordered:{type:Boolean,default:void 0},checked:Boolean,checkable:Boolean,strong:Boolean,triggerClickOnClose:Boolean,onClose:[Array,Function],onMouseenter:Function,onMouseleave:Function,"onUpdate:checked":Function,onUpdateChecked:Function,internalCloseFocusable:{type:Boolean,default:!0},internalCloseIsButtonTag:{type:Boolean,default:!0},onCheckedChange:Function},V=N(`n-tag`),H=e({name:`Tag`,props:B,slots:Object,setup(e){let t=d(null),{mergedBorderedRef:n,mergedClsPrefixRef:i,inlineThemeDisabled:a,mergedRtlRef:c,mergedComponentPropsRef:f}=j(e),p=y(()=>e.size||f?.value?.Tag?.size||`medium`),m=v(`Tag`,`-tag`,R,I,e,i);s(V,{roundRef:u(e,`round`)});function h(){if(!e.disabled&&e.checkable){let{checked:t,onCheckedChange:n,onUpdateChecked:r,"onUpdate:checked":i}=e;r&&r(!t),i&&i(!t),n&&n(!t)}}function g(t){if(e.triggerClickOnClose||t.stopPropagation(),!e.disabled){let{onClose:n}=e;n&&r(n,t)}}let _={setTextContent(e){let{value:n}=t;n&&(n.textContent=e)}},b=o(`Tag`,c,i),x=y(()=>{let{type:t,color:{color:r,textColor:i}={}}=e,a=p.value,{common:{cubicBezierEaseInOut:o},self:{padding:s,closeMargin:c,borderRadius:l,opacityDisabled:u,textColorCheckable:d,textColorHoverCheckable:f,textColorPressedCheckable:h,textColorChecked:g,colorCheckable:_,colorHoverCheckable:v,colorPressedCheckable:y,colorChecked:b,colorCheckedHover:x,colorCheckedPressed:S,closeBorderRadius:C,fontWeightStrong:w,[D(`colorBordered`,t)]:T,[D(`closeSize`,a)]:E,[D(`closeIconSize`,a)]:O,[D(`fontSize`,a)]:A,[D(`height`,a)]:j,[D(`color`,t)]:M,[D(`textColor`,t)]:N,[D(`border`,t)]:P,[D(`closeIconColor`,t)]:F,[D(`closeIconColorHover`,t)]:I,[D(`closeIconColorPressed`,t)]:L,[D(`closeColorHover`,t)]:R,[D(`closeColorPressed`,t)]:z}}=m.value,B=k(c);return{"--n-font-weight-strong":w,"--n-avatar-size-override":`calc(${j} - 8px)`,"--n-bezier":o,"--n-border-radius":l,"--n-border":P,"--n-close-icon-size":O,"--n-close-color-pressed":z,"--n-close-color-hover":R,"--n-close-border-radius":C,"--n-close-icon-color":F,"--n-close-icon-color-hover":I,"--n-close-icon-color-pressed":L,"--n-close-icon-color-disabled":F,"--n-close-margin-top":B.top,"--n-close-margin-right":B.right,"--n-close-margin-bottom":B.bottom,"--n-close-margin-left":B.left,"--n-close-size":E,"--n-color":r||(n.value?T:M),"--n-color-checkable":_,"--n-color-checked":b,"--n-color-checked-hover":x,"--n-color-checked-pressed":S,"--n-color-hover-checkable":v,"--n-color-pressed-checkable":y,"--n-font-size":A,"--n-height":j,"--n-opacity-disabled":u,"--n-padding":s,"--n-text-color":i||N,"--n-text-color-checkable":d,"--n-text-color-checked":g,"--n-text-color-hover-checkable":f,"--n-text-color-pressed-checkable":h}}),S=a?T(`tag`,y(()=>{let t=``,{type:r,color:{color:i,textColor:a}={}}=e;return t+=r[0],t+=p.value[0],i&&(t+=`a${l(i)}`),a&&(t+=`b${l(a)}`),n.value&&(t+=`c`),t}),x,e):void 0;return{..._,rtlEnabled:b,mergedClsPrefix:i,contentRef:t,mergedBordered:n,handleClick:h,handleCloseClick:g,cssVars:a?void 0:x,themeClass:S?.themeClass,onRender:S?.onRender}},render(){let{mergedClsPrefix:e,rtlEnabled:t,closable:r,color:{borderColor:o}={},round:s,onRender:c,$slots:l}=this;c?.();let u=i(l.avatar,t=>t&&(a(),h(`div`,{class:S(`${e}-tag__avatar`)},[b(()=>t)],2))),d=i(l.icon,t=>t&&(a(),h(`div`,{class:S(`${e}-tag__icon`)},[b(()=>t)],2)));return a(),h(`div`,{class:S([`${e}-tag`,this.themeClass,{[`${e}-tag--rtl`]:t,[`${e}-tag--strong`]:this.strong,[`${e}-tag--disabled`]:this.disabled,[`${e}-tag--checkable`]:this.checkable,[`${e}-tag--checked`]:this.checkable&&this.checked,[`${e}-tag--round`]:s,[`${e}-tag--avatar`]:u,[`${e}-tag--icon`]:d,[`${e}-tag--closable`]:r}]),style:n(this.cssVars),onClick:this.handleClick,onMouseenter:this.onMouseenter,onMouseleave:this.onMouseleave},[b(()=>d||u),p(`span`,{class:S(`${e}-tag__content`),ref:`contentRef`},[b(()=>this.$slots.default?.())],2),!this.checkable&&r?(a(),A(M,{key:0,clsPrefix:e,class:S(`${e}-tag__close`),disabled:this.disabled,onClick:this.handleCloseClick,focusable:this.internalCloseFocusable,round:s,isButtonTag:this.internalCloseIsButtonTag,absolute:!0},null,8,[`clsPrefix`,`class`,`disabled`,`onClick`,`focusable`,`round`,`isButtonTag`])):b(()=>null),!this.checkable&&this.mergedBordered?(a(),h(`div`,{key:2,class:S(`${e}-tag__border`),style:n({borderColor:o})},null,6)):b(()=>null)],46,z)}});function U(){let e=t(m,null);return e===null&&g(`use-dialog`,`No outer <n-dialog-provider /> founded.`),e}var W=C({name:`refresh-cw`,size:24,node:[[`path`,{d:`M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8`,key:`v9h5vc`}],[`path`,{d:`M21 3v5h-5`,key:`1q7to0`}],[`path`,{d:`M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16`,key:`3uifl3`}],[`path`,{d:`M8 16H3v5`,key:`1cv678`}]]});export{I as a,L as i,U as n,H as r,W as t};